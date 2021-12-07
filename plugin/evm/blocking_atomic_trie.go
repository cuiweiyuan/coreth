// (c) 2020-2021, Ava Labs, Inc.
// See the file LICENSE for licensing terms.

package evm

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/database/versiondb"

	"github.com/ava-labs/coreth/trie"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/coreth/fastsync/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	commitHeightInterval = uint64(4096)
)

var (
	atomicIndexDBPrefix     = []byte("atomicIndexDB")
	atomicIndexMetaDBPrefix = []byte("atomicIndexMetaDB")
	lastCommittedKey        = []byte("atomicTrieLastCommittedBlock")
)

// blockingAtomicTrie implements the types.AtomicTrie interface
// using the eth trie.Trie implementation
type blockingAtomicTrie struct {
	commitHeightInterval uint64              // commit interval, same as commitHeightInterval by default
	db                   *versiondb.Database // Underlying database
	metadataDB           database.Database   // Underlying database containing the atomic trie metadata
	atomicTrieDB         database.Database   // Underlying database containing the atomic trie
	trieDB               *trie.Database      // Trie database
	trie                 *trie.Trie          // Atomic trie.Trie mapping key (height+blockchainID) and value (RLP encoded atomic.Requests)
	repo                 AtomicTxRepository
	lastCommittedHash    common.Hash // trie root hash of the most recent commit
	lastCommittedHeight  uint64      // index height of the most recent commit
	codec                codec.Manager
	log                  log.Logger // struct logger
}

// NewBlockingAtomicTrie returns a new instance of a blockingAtomicTrie configured with default commitHeightInterval.
// The trie is initialized before it is returned.
func NewBlockingAtomicTrie(db *versiondb.Database, repo AtomicTxRepository, codec codec.Manager, lastAcceptedHeight uint64) (types.AtomicTrie, error) {
	return newBlockingAtomicTrie(db, repo, codec, lastAcceptedHeight, commitHeightInterval)
}

// newBlockingAtomicTrie is to be used for testing, allows setting of custom commitHeightInterval.
// Also initializes the trie before returning it.
func newBlockingAtomicTrie(
	db *versiondb.Database, repo AtomicTxRepository, codec codec.Manager, lastAcceptedHeight uint64, commitHeightInterval uint64,
) (types.AtomicTrie, error) {
	atomicTrieDB := prefixdb.New(atomicIndexDBPrefix, db)
	metadataDB := prefixdb.New(atomicIndexMetaDBPrefix, db)
	root, height, err := lastCommittedRootIfExists(metadataDB)
	if err != nil {
		return nil, err
	}

	triedb := trie.NewDatabase(Database{atomicTrieDB})
	t, err := trie.New(root, triedb)
	if err != nil {
		return nil, err
	}

	atomicTrie := &blockingAtomicTrie{
		commitHeightInterval: commitHeightInterval,
		db:                   db,
		atomicTrieDB:         atomicTrieDB,
		metadataDB:           metadataDB,
		trieDB:               triedb,
		trie:                 t,
		repo:                 repo,
		codec:                codec,
		lastCommittedHash:    root,
		lastCommittedHeight:  height,
		log:                  log.New("c", "blockingAtomicTrie"),
	}
	return atomicTrie, atomicTrie.initialize(lastAcceptedHeight)
}

// lastCommittedRootIfExists returns the last committed trie root and height if it exists
// else returns empty common.Hash{} and 0
// returns error only if there are issues with the underlying data store
// or if values present in the database are not as expected
func lastCommittedRootIfExists(db database.Database) (common.Hash, uint64, error) {
	// read the last committed entry if it exists and set the root hash
	lastCommittedHeightBytes, err := db.Get(lastCommittedKey)
	switch {
	// err type does not match database.ErrorNotFound, check `.Error()` instead
	case err == database.ErrNotFound:
		return common.Hash{}, 0, nil
	case err != nil:
		return common.Hash{}, 0, err
	case len(lastCommittedHeightBytes) != wrappers.LongLen:
		return common.Hash{}, 0, fmt.Errorf("expected value of lastCommittedKey to be %d but was %d", wrappers.LongLen, len(lastCommittedHeightBytes))
	default:
		height := binary.BigEndian.Uint64(lastCommittedHeightBytes)
		hash, err := db.Get(lastCommittedHeightBytes)
		if err != nil {
			return common.Hash{}, 0, err
		}
		return common.BytesToHash(hash), height, nil
	}
}

// nearestCommitHeight given blockNumber calculates and returns commitHeight such that
// commitHeight is less than or equal to blockNumber, commitHeight+commitInterval is
// greater than blockNumber and commitHeight is divisible by commitInterval
func nearestCommitHeight(blockNumber uint64, commitInterval uint64) uint64 {
	return blockNumber - (blockNumber % commitInterval)
}

// initializes the atomic trie using the atomic repository height index.
// Iterating from the last indexed height to lastAcceptedBlockNumber, making a single commit at the
// most recent height divisible by the commitInterval.
// Subsequent updates to this trie are made using the Index call as blocks are accepted.
func (b *blockingAtomicTrie) initialize(lastAcceptedBlockNumber uint64) error {
	startTime := time.Now()
	b.log.Info("initializing atomic trie", "lastAcceptedBlockNumber", lastAcceptedBlockNumber)
	// commitHeight is the highest block that can be committed i.e. is divisible by b.commitHeightInterval
	commitHeight := nearestCommitHeight(lastAcceptedBlockNumber, b.commitHeightInterval)
	uncommittedOpsMap := make(map[uint64]map[ids.ID]*atomic.Requests, lastAcceptedBlockNumber-commitHeight)

	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, b.lastCommittedHeight)
	// iterate by height, from lastCommittedHeight to the lastAcceptedBlockNumber
	iter := b.repo.IterateByHeight(heightBytes)
	defer iter.Release()

	preCommitBlockIndexed := 0
	postCommitTxIndexed := 0
	lastUpdate := time.Now()
	for iter.Next() {
		if err := iter.Error(); err != nil {
			return err
		}

		// Get height for this iteration + transactions
		// iterate over the transactions, indexing them if the height is < commit height
		// otherwise, add the atomic operations from the transaction to the uncommittedOpsMap
		height := binary.BigEndian.Uint64(iter.Key())
		txs, err := ExtractAtomicTxs(iter.Value(), true, b.codec)
		if err != nil {
			return err
		}

		// combine atomic operations from all transactions at this block height
		combinedOps := make(map[ids.ID]*atomic.Requests)
		for _, tx := range txs {
			id, reqs, err := tx.Accept()
			ops := map[ids.ID]*atomic.Requests{id: reqs}
			if err != nil {
				return err
			}

			for chainID, ops := range ops {
				if chainOps, exists := combinedOps[chainID]; exists {
					chainOps.PutRequests = append(chainOps.PutRequests, ops.PutRequests...)
					chainOps.RemoveRequests = append(chainOps.RemoveRequests, ops.RemoveRequests...)
				} else {
					combinedOps[id] = ops
				}
			}
		}

		// if height is greater than commit height, add it to the map so that we can write it later
		// this is to ensure we have all the data before the commit height so that we can commit the
		// trie
		if height > commitHeight {
			uncommittedOpsMap[height] = combinedOps
		} else {
			if err = b.updateTrie(height, combinedOps); err != nil {
				return err
			}
			preCommitBlockIndexed++
		}

		if time.Since(lastUpdate) > 30*time.Second {
			b.log.Info("imported entries into atomic trie pre-commit", "heightsIndexed", preCommitBlockIndexed)
			lastUpdate = time.Now()
		}
	}

	// skip commit in case of early height
	// should never happen in production since height is greater than 4096
	if lastAcceptedBlockNumber < b.commitHeightInterval {
		return nil
	}

	// now that all heights < commitHeight have been processed
	// commit the trie
	hash, err := b.commit(commitHeight)
	if err != nil {
		return err
	}

	// commit to underlying versiondb
	if err := b.db.Commit(); err != nil {
		return err
	}
	b.lastCommittedHash = hash
	b.lastCommittedHeight = commitHeight
	b.log.Info("committed trie", "hash", hash, "indexHeight", commitHeight)

	// process uncommitted ops for heights > commitHeight
	for height, ops := range uncommittedOpsMap {
		if err = b.updateTrie(height, ops); err != nil {
			return err
		}

		postCommitTxIndexed++
		if time.Since(lastUpdate) > 30*time.Second {
			b.log.Info("imported entries into atomic trie post-commit", "entriesIndexed", postCommitTxIndexed)
			lastUpdate = time.Now()
		}
	}

	b.log.Info(
		"finished initializing atomic trie",
		"lastAcceptedBlockNumber", lastAcceptedBlockNumber,
		"preCommitEntriesIndexed", preCommitBlockIndexed, "postCommitEntriesIndexed", postCommitTxIndexed,
		"time", time.Since(startTime),
	)
	return nil
}

// Index updates the trie with entries in atomicOps
// A non-empty hash is returned if the trie was committed (the height is divisible by commitInterval)
// This function updates the following:
// - heightBytes => trie root hash (if the trie was committed)
// - lastCommittedBlock => height (if the trie was committed)
func (b *blockingAtomicTrie) Index(height uint64, atomicOps map[ids.ID]*atomic.Requests) (common.Hash, error) {

	// disallow going backwards
	if height < b.lastCommittedHeight {
		return common.Hash{}, fmt.Errorf("height %d must be after last committed height %d", height, b.lastCommittedHeight)
	}

	// disallow going ahead too far
	nextCommitHeight := b.lastCommittedHeight + b.commitHeightInterval
	if height > nextCommitHeight {
		return common.Hash{}, fmt.Errorf("height %d not within the next commit height %d", height, nextCommitHeight)
	}

	if err := b.updateTrie(height, atomicOps); err != nil {
		return common.Hash{}, err
	}

	if height%b.commitHeightInterval == 0 {
		return b.commit(height)
	}
	return common.Hash{}, nil
}

// commit the underlying trie, generating a trie root hash
// assumes that the caller is aware of the commit rules i.e. the height being within commitInterval
// returns the trie root from the commit
func (b *blockingAtomicTrie) commit(height uint64) (common.Hash, error) {
	hash, _, err := b.trie.Commit(nil)
	if err != nil {
		return common.Hash{}, err
	}

	b.log.Debug("committed atomic trie", "hash", hash.String(), "height", height)
	if err := b.trieDB.Commit(hash, false, nil); err != nil {
		return common.Hash{}, err
	}

	// all good here, update the heightBytes
	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, height)

	// now save the trie hash against the height it was committed at
	if err = b.metadataDB.Put(heightBytes, hash[:]); err != nil {
		return common.Hash{}, err
	}

	// update lastCommittedKey with the current height
	if err = b.metadataDB.Put(lastCommittedKey, heightBytes); err != nil {
		return common.Hash{}, err
	}

	b.lastCommittedHash = hash
	b.lastCommittedHeight = height
	return hash, nil
}

func (b *blockingAtomicTrie) updateTrie(height uint64, atomicOps map[ids.ID]*atomic.Requests) error {
	for blockchainID, requests := range atomicOps {
		valueBytes, err := rlp.EncodeToBytes(requests)
		if err != nil {
			// highly unlikely but possible if atomic.Element
			// has a change that is unsupported by the RLP encoder
			return err
		}

		// key is [height]+[blockchainID]
		keyPacker := wrappers.Packer{Bytes: make([]byte, wrappers.LongLen+common.HashLength)}
		keyPacker.PackLong(height)
		keyPacker.PackFixedBytes(blockchainID[:])
		b.trie.Update(keyPacker.Bytes, valueBytes)
	}
	return nil
}

// LastCommitted returns the last committed trie hash and last committed height
func (b *blockingAtomicTrie) LastCommitted() (common.Hash, uint64) {
	return b.lastCommittedHash, b.lastCommittedHeight
}

// Iterator returns a types.AtomicTrieIterator that iterates the trie from the given
// atomic trie root, starting at the specified height
func (b *blockingAtomicTrie) Iterator(root common.Hash, startHeight uint64) (types.AtomicTrieIterator, error) {
	var startKey []byte
	if startHeight > 0 {
		startKey = make([]byte, wrappers.LongLen)
		binary.BigEndian.PutUint64(startKey, startHeight)
	}

	t, err := trie.New(root, b.trieDB)
	if err != nil {
		return nil, err
	}

	iter := trie.NewIterator(t.NodeIterator(startKey))
	return NewAtomicTrieIterator(iter), iter.Err
}

func (b *blockingAtomicTrie) TrieDB() *trie.Database {
	return b.trieDB
}

// Root returns hash if it exists at specified height
// if trie was not committed at provided height, it returns
// common.Hash{} instead
func (b *blockingAtomicTrie) Root(height uint64) (common.Hash, error) {
	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, height)

	hash, err := b.metadataDB.Get(heightBytes)
	switch {
	case err == database.ErrNotFound:
		return common.Hash{}, nil
	case err != nil:
		return common.Hash{}, err
	default:
		return common.BytesToHash(hash), nil
	}
}
