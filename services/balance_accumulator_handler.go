package services

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"

	"github.com/ava-labs/ortelius/services/db"

	"github.com/gocraft/dbr/v2"
)

var RowLintValue = 100
var RowLimit = fmt.Sprintf("%d", RowLintValue)
var updTimeout = 10 * time.Second

type processType uint32

var processTypeIn processType = 1
var processTypeOut processType = 2

type BalancerAccumulateHandler struct {
	running int64
	lock    sync.Mutex
	Log     logging.Logger
}

func (a *BalancerAccumulateHandler) Run(conns *Connections, persist Persist, sc *Control) {
	if atomic.LoadInt64(&a.running) != 0 {
		return
	}

	a.lock.Lock()
	defer a.lock.Unlock()
	if atomic.LoadInt64(&a.running) != 0 {
		return
	}

	atomic.AddInt64(&a.running, 1)
	go func() {
		defer func() {
			atomic.AddInt64(&a.running, -1)
		}()
		var err error
		for {
			err = a.Accumulate(conns, persist)
			if err == nil || !strings.Contains(err.Error(), db.DeadlockDBErrorMessage) {
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		if err != nil {
			sc.Log.Warn("Accumulate %v", err)
		}
	}()
}

func (a *BalancerAccumulateHandler) Accumulate(conns *Connections, persist Persist) error {
	job := conns.Stream().NewJob("accumulate")
	sess := conns.DB().NewSessionForEventReceiver(job)

	icnt := 0
	for ; icnt < 10; icnt++ {
		for {
			cnt, err := a.processOutputs(processTypeOut, sess, persist)
			if err != nil {
				return err
			}
			if cnt > 0 {
				icnt = 0
			}
			if cnt < RowLintValue {
				break
			}
		}
		for {
			cnt, err := a.processOutputs(processTypeIn, sess, persist)
			if err != nil {
				return err
			}
			if cnt > 0 {
				icnt = 0
			}
			if cnt < RowLintValue {
				break
			}
		}
		for {
			cnt, err := a.processTransactions(sess, persist)
			if err != nil {
				return err
			}
			if cnt > 0 {
				icnt = 0
			}
			if cnt < RowLintValue {
				break
			}
		}
	}

	return nil
}

func (a *BalancerAccumulateHandler) processOutputs(typ processType, sess *dbr.Session, persist Persist) (int, error) {
	ctx, cancelCTX := context.WithTimeout(context.Background(), updTimeout)
	defer cancelCTX()

	var err error
	var rowdata []*OutputAddressAccumulate

	var dbTx *dbr.Tx
	dbTx, err = sess.Begin()
	if err != nil {
		return 0, err
	}
	defer dbTx.RollbackUnlessCommitted()

	switch typ {
	case processTypeOut:
		_, err = dbTx.SelectBySql("select output_addresses_accumulate.id,output_addresses_accumulate.address "+
			"from output_addresses_accumulate "+
			"join avm_outputs on "+
			"  output_addresses_accumulate.id = avm_outputs.id "+
			"where "+
			"output_addresses_accumulate.processed_out = 0 "+
			"limit "+RowLimit+" "+
			"for update").
			LoadContext(ctx, &rowdata)
		if err != nil {
			return 0, err
		}
	case processTypeIn:
		_, err = dbTx.SelectBySql("select output_addresses_accumulate.id,output_addresses_accumulate.address "+
			"from output_addresses_accumulate "+
			"join avm_outputs on "+
			"  output_addresses_accumulate.id = avm_outputs.id "+
			"join avm_outputs_redeeming on "+
			"  output_addresses_accumulate.id = avm_outputs_redeeming.id "+
			"where "+
			"output_addresses_accumulate.processed_in = 0 "+
			"limit "+RowLimit+" "+
			"for update ").
			LoadContext(ctx, &rowdata)
		if err != nil {
			return 0, err
		}
	}

	if len(rowdata) == 0 {
		return 0, nil
	}

	for _, row := range rowdata {
		balances := []*AccumulateBalances{}

		_, err = dbTx.Select("avm_outputs.chain_id",
			"avm_output_addresses.address",
			"avm_outputs.asset_id",
			"count(distinct(avm_outputs.transaction_id)) as transaction_count",
			"sum(avm_outputs.amount) as total_received",
			"sum(avm_outputs.amount) as total_sent",
		).From("avm_outputs").
			Join("avm_output_addresses", "avm_outputs.id = avm_output_addresses.output_id").
			Where("avm_outputs.id=? and avm_output_addresses.address=? ", row.ID, row.Address).
			GroupBy("avm_outputs.chain_id", "avm_output_addresses.address", "avm_outputs.asset_id").
			LoadContext(ctx, &balances)
		if err != nil {
			return 0, err
		}

		if len(balances) == 0 {
			a.Log.Info("invalid balance %s %s on %d", row.ID, row.Address, typ)
			continue
		}

		accumulateBalanceIds := []string{}
		for _, b := range balances {
			err = b.ComputeID()
			if err != nil {
				return 0, err
			}
			accumulateBalanceIds = append(accumulateBalanceIds, b.ID)

			err = persist.InsertAccumulateBalances(ctx, dbTx, b)
			if err != nil {
				return 0, err
			}
		}

		balancesLocked := []*AccumulateBalances{}
		_, err = dbTx.SelectBySql("select id "+
			"from accumulate_balances "+
			"where id in ? "+
			"for update", accumulateBalanceIds).
			LoadContext(ctx, &balancesLocked)
		if err != nil {
			return 0, err
		}

		for _, b := range balances {
			switch typ {
			case processTypeOut:
				_, err = dbTx.UpdateBySql("update accumulate_balances "+
					"set "+
					"utxo_count = utxo_count+1, "+
					"total_received = total_received+"+b.TotalReceived+" "+
					"where id=? "+
					"", b.ID).
					ExecContext(ctx)
				if err != nil {
					return 0, err
				}
			case processTypeIn:
				_, err = dbTx.UpdateBySql("update accumulate_balances "+
					"set "+
					"utxo_count = utxo_count-1, "+
					"total_sent = total_sent+"+b.TotalSent+" "+
					"where id=? "+
					"", b.ID).
					ExecContext(ctx)
				if err != nil {
					return 0, err
				}
			}
		}

		upd := ""
		switch typ {
		case processTypeOut:
			upd = "processed_out = 1 "
		case processTypeIn:
			upd = "processed_in = 1 "
		}
		_, err = dbTx.UpdateBySql("update output_addresses_accumulate "+
			"set "+upd+" "+
			"where id=? and address=? "+
			"", row.ID, row.Address).
			ExecContext(ctx)
		if err != nil {
			return 0, err
		}
	}

	if err = dbTx.Commit(); err != nil {
		return 0, err
	}

	return len(rowdata), nil
}

func (a *BalancerAccumulateHandler) processTransactions(sess *dbr.Session, persist Persist) (int, error) {
	ctx, cancelCTX := context.WithTimeout(context.Background(), updTimeout)
	defer cancelCTX()

	var err error
	var rowdata []*OutputTxsAccumulate

	var dbTx *dbr.Tx
	dbTx, err = sess.Begin()
	if err != nil {
		return 0, err
	}
	defer dbTx.RollbackUnlessCommitted()

	_, err = dbTx.SelectBySql("select id,chain_id,asset_id,address,transaction_id "+
		"from output_txs_accumulate "+
		"where processed = 0 "+
		"limit "+RowLimit+" "+
		"for update").
		LoadContext(ctx, &rowdata)
	if err != nil {
		return 0, err
	}

	if len(rowdata) == 0 {
		return 0, nil
	}

	balances := []*AccumulateBalances{}
	accumulateBalanceIds := []string{}
	for _, row := range rowdata {
		b := &AccumulateBalances{
			ChainID: row.ChainID,
			AssetID: row.AssetID,
			Address: row.Address,
		}
		err = b.ComputeID()
		if err != nil {
			return 0, err
		}
		accumulateBalanceIds = append(accumulateBalanceIds, b.ID)
		err = persist.InsertAccumulateBalances(ctx, dbTx, b)
		if err != nil {
			return 0, err
		}
		balances = append(balances, b)
	}

	balancesLocked := []*AccumulateBalances{}
	_, err = dbTx.SelectBySql("select id "+
		"from accumulate_balances "+
		"where id in ? "+
		"for update", accumulateBalanceIds).
		LoadContext(ctx, &balancesLocked)
	if err != nil {
		return 0, err
	}

	for _, b := range balances {
		_, err = dbTx.UpdateBySql("update accumulate_balances "+
			"set "+
			"transaction_count = transaction_count+1 "+
			"where id=? "+
			"", b.ID).
			ExecContext(ctx)
		if err != nil {
			return 0, err
		}
	}

	for _, row := range rowdata {
		_, err = dbTx.UpdateBySql("update output_txs_accumulate "+
			"set processed=1 "+
			"where id=? "+
			"", row.ID).
			ExecContext(ctx)
		if err != nil {
			return 0, err
		}
	}

	if err = dbTx.Commit(); err != nil {
		return 0, err
	}

	return len(rowdata), nil
}