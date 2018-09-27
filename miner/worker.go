package miner

import (
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"srcd/common/common"
	"srcd/consensus"
	"srcd/core"
	"srcd/core/blockchain"
	"srcd/core/types"
	"srcd/log"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 5

	// blockRecommitInterval is the time interval to recreate the mining block with
	// any newly arrived transactions.
	blockRecommitInterval = 3 * time.Second
)

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer   types.Signer
	tcount   int            // tx count in cycle
	header   *types.Header
	txs      []*types.Transaction
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

type newWorkReq struct {
	interrupt *int32
	noempty   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	engine        consensus.Engine
	server        Backend
	chain         *blockchain.BlockChain

	// Subscriptions
	mux           *event.TypeMux
	txsCh         chan core.NewTxsEvent
	// txsSub        event.Subscription
	chainHeadCh   chan core.ChainHeadEvent
	// chainHeadSub event.Subscription

	// Channels
	newWorkCh     chan *newWorkReq
	taskCh        chan *task
	resultCh      chan *task
	startCh       chan struct{}
	exitCh        chan struct{}

	current       *environment        // An environment for current running cycle.
	unconfirmed   *unconfirmedBlocks  // A set of locally mined blocks pending canonicalness confirmations.

	mu            sync.RWMutex        // The lock used to protect the coinbase and extra fields
	coinbase      common.Address
	extra         []byte

	snapshotMu    sync.RWMutex        // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block

	// atomic status counters
	running       int32               // The indicator whether the consensus engine is running or not.

	// Test hooks
	newTaskHook   func(*task)         // Method to call upon receiving a new sealing task
	skipSealHook  func(*task) bool    // Method to decide whether skipping the sealing.
	fullTaskHook  func()              // Method to call before pushing the full sealing task
}

func newWorker(engine consensus.Engine, server Backend, mux *event.TypeMux) *worker {
	worker := &worker{
		engine:         engine,
		server:         server,
		mux:            mux,
		chain:          server.BlockChain(),
		unconfirmed:    newUnconfirmedBlocks(server.BlockChain(), miningLogAtDepth),
		txsCh:          make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:    make(chan core.ChainHeadEvent, chainHeadChanSize),
		newWorkCh:      make(chan *newWorkReq),
		taskCh:         make(chan *task),
		resultCh:       make(chan *task, resultQueueSize),
		exitCh:         make(chan struct{}),
		startCh:        make(chan struct{}, 1),
	}
	// // Subscribe NewTxsEvent for tx pool
	// worker.txsSub = server.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// // Subscribe events for blockchain
	// worker.chainHeadSub = server.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)

	go worker.mainLoop()
	go worker.newWorkLoop()
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	worker.startCh <- struct{}{}

	return worker
}

// setCoinbase sets the coinbase used to initialize the block coinbase field.
func (w *worker) setCoinbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// pending returns pending block.
func (w *worker) pending() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker and cleans up buffered channels.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	close(w.exitCh)
	// Clean up buffered channels
	for empty := false; !empty; {
		select {
		case <-w.resultCh:
		default:
			empty = true
		}
	}
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop() {
	var interrupt *int32

	timer := time.NewTimer(0)
	<-timer.C // discard the initial tick

	// recommit aborts in-flight transaction execution with given signal and resubmits a new one.
	recommit := func(noempty bool, s int32) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		w.newWorkCh <- &newWorkReq{interrupt: interrupt, noempty: noempty}
		timer.Reset(blockRecommitInterval)
	}

	for {
		select {
		case <-w.startCh:
			recommit(false, commitInterruptNewHead)

		case <-w.chainHeadCh:
			recommit(false, commitInterruptNewHead)

		case <-timer.C:
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() {
				recommit(true, commitInterruptResubmit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	// defer w.txsSub.Unsubscribe()
	// defer w.chainHeadSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitNewWork(req.interrupt, req.noempty)

		case ev := <-w.txsCh:
			// Apply transactions to the pending state if we're not mining.
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current mining block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				w.mu.RLock()
				coinbase := w.coinbase
				w.mu.RUnlock()

				txs := make(map[common.Address]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc] = append(txs[acc], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs)
				w.commitTransactions(txset, coinbase, nil)
				w.updateSnapshot()
			}

		// System stopped
		case <-w.exitCh:
			return
		// case <-w.txsSub.Err():
			// return
		// case <-w.chainHeadSub.Err():
			// return
		}
	}
}

// seal pushes a sealing task to consensus engine and submits the result.
func (w *worker) seal(t *task, stop <-chan struct{}) {
	var (
		err error
		res *task
	)

	if w.skipSealHook != nil && w.skipSealHook(t) {
		return
	}

	if t.block, err = w.engine.Seal(w.chain, t.block, stop); t.block != nil {
		log.Info("Successfully sealed new block", "number", t.block.Number(), "hash", t.block.Hash(),
			"elapsed", common.PrettyDuration(time.Since(t.createdAt)))
		res = t
	} else {
		if err != nil {
			log.Warn("Block sealing failed", "err", err)
		}
		res = nil
	}
	select {
	case w.resultCh <- res:
	case <-w.exitCh:
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var stopCh chan struct{}

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			interrupt()
			stopCh = make(chan struct{})
			go w.seal(task, stopCh)
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case result := <-w.resultCh:
			if result == nil {
				continue
			}
			block := result.block

			// Commit block to database.
			stat, err := w.chain.WriteBlockWithState(block, result.receipts, result.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			// // Broadcast the block and announce chain insertion event
			// w.mux.Post(core.NewMinedBlockEvent{Block: block})
			// var (
				// events []interface{}
				// logs   = result.state.Logs()
			// )
			// switch stat {
			// case core.CanonStatTy:
				// events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
				// events = append(events, core.ChainHeadEvent{Block: block})
			// case core.SideStatTy:
				// events = append(events, core.ChainSideEvent{Block: block})
			// }
			// w.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	env := &environment{
		signer:    types.NewMasterSigner(),
		header:    header,
	}

	// // when 08 is processed ancestors contain 07 (quick block)
	// for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		// env.family.Add(ancestor.Hash())
		// env.ancestors.Add(ancestor.Hash())
	// }

	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	w.current = env
	return nil
}

// updateSnapshot updates pending snapshot block.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot() {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		w.current.header,
		w.current.txs,
	)
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) error {
	// receipt, _, err := core.ApplyTransaction(w.config, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed)
	// if err != nil {
		// w.current.state.RevertToSnapshot(snap)
		// return err
	// }

	w.current.txs = append(w.current.txs, tx)

	return nil
}

func (w *worker) commitTransactions(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) bool {
	// Short circuit if current is nil
	if w.current == nil {
		return true
	}

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		// TODO(rjl493456442) give feedback to newWorkLoop to adjust resubmit interval if it is too short.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}

		// check tx fees ...

		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected.
		if tx.Protected() {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash())

			txs.Pop()
			continue
		}

		err := w.commitTransaction(tx, coinbase)
		switch err {
		case nil:
			// Everything ok, shift in the next transaction from the same account
			w.current.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	return false
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, noempty bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	tstart := time.Now()
	parent := w.chain.CurrentBlock()

	tstamp := tstart.Unix()
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}
	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); tstamp > now+1 {
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		Extra:      w.extra,
		Time:       big.NewInt(tstamp),
	}
	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without coinbase")
			return
		}
		header.Coinbase = w.coinbase
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}

	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}

	if !noempty {
		// Create an empty block based on temporary copied state for sealing in advance without waiting block
		// execution finished.
		w.commit(nil, false, tstart)
	}

	// Fill the block with all available pending transactions.
	pending, err := w.server.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}
	// Short circuit if there is no available pending transactions
	if len(pending) == 0 {
		w.updateSnapshot()
		return
	}
	txs := types.NewTransactionsByPriceAndNonce(w.current.signer, pending)
	if w.commitTransactions(txs, w.coinbase, interrupt) {
		return
	}

	w.commit(w.fullTaskHook, true, tstart)
}

// commit assembles the final block and commits new work if consensus engine is running.
func (w *worker) commit(interval func(), update bool, start time.Time) error {
	block, err := w.engine.Finalize(w.current.header, w.current.txs)
	if err != nil {
		return err
	}

	if w.isRunning() {
		if interval != nil {
			interval()
		}
		select {
		case w.taskCh <- &task{block: block, createdAt: time.Now()}:
			w.unconfirmed.Shift(block.NumberU64() - 1)

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}
	if update {
		w.updateSnapshot()
	}
	return nil
}
