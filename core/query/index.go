package query

import (
	"context"
	"encoding/json"

	"github.com/lib/pq"

	"chain/database/pg"
	"chain/errors"
	"chain/protocol/bc"
)

const (
	// TxPinName is used to identify the pin associated
	// with the transaction block processor.
	TxPinName = "tx"
)

// Annotator describes a function capable of adding annotations
// to transactions, inputs and outputs.
type Annotator func(ctx context.Context, txs []*AnnotatedTx) error

// RegisterAnnotator adds an additional annotator capable of mutating
// the annotated transaction object.
func (ind *Indexer) RegisterAnnotator(annotator Annotator) {
	ind.annotators = append(ind.annotators, annotator)
}

func (ind *Indexer) ProcessBlocks(ctx context.Context) {
	if ind.pinStore == nil {
		return
	}
	ind.pinStore.ProcessBlocks(ctx, ind.c, TxPinName, ind.IndexTransactions)
}

// IndexTransactions is registered as a block callback on the Chain. It
// saves all annotated transactions to the database.
func (ind *Indexer) IndexTransactions(ctx context.Context, b *bc.Block) error {
	<-ind.pinStore.PinWaiter("asset", b.Height)

	err := ind.insertBlock(ctx, b)
	if err != nil {
		return err
	}

	txs, err := ind.insertAnnotatedTxs(ctx, b)
	if err != nil {
		return err
	}

	return ind.insertAnnotatedOutputs(ctx, b, txs)
}

func (ind *Indexer) insertBlock(ctx context.Context, b *bc.Block) error {
	const q = `
		INSERT INTO query_blocks (height, timestamp) VALUES($1, $2)
		ON CONFLICT (height) DO NOTHING
	`
	_, err := ind.db.Exec(ctx, q, b.Height, b.TimestampMS)
	return errors.Wrap(err, "inserting block timestamp")
}

func (ind *Indexer) insertAnnotatedTxs(ctx context.Context, b *bc.Block) ([]*AnnotatedTx, error) {
	var (
		hashes              = pq.ByteaArray(make([][]byte, 0, len(b.Transactions)))
		positions           = pg.Uint32s(make([]uint32, 0, len(b.Transactions)))
		annotatedTxs        = pq.StringArray(make([]string, 0, len(b.Transactions)))
		annotatedTxsDecoded = make([]*AnnotatedTx, 0, len(b.Transactions))
	)
	for pos, tx := range b.Transactions {
		hashes = append(hashes, tx.Hash[:])
		positions = append(positions, uint32(pos))
		annotatedTxsDecoded = append(annotatedTxsDecoded, buildAnnotatedTransaction(tx, b, uint32(pos)))
	}

	for _, annotator := range ind.annotators {
		err := annotator(ctx, annotatedTxsDecoded)
		if err != nil {
			return nil, errors.Wrap(err, "adding external annotations")
		}
	}
	localAnnotator(ctx, annotatedTxsDecoded)

	for _, decoded := range annotatedTxsDecoded {
		b, err := json.Marshal(decoded)
		if err != nil {
			return nil, err
		}
		annotatedTxs = append(annotatedTxs, string(b))
	}

	// Save the annotated txs to the database.
	const insertQ = `
		INSERT INTO annotated_txs(block_height, tx_pos, tx_hash, data)
		SELECT $1, unnest($2::integer[]), unnest($3::bytea[]), unnest($4::jsonb[])
		ON CONFLICT (block_height, tx_pos) DO NOTHING;
	`
	_, err := ind.db.Exec(ctx, insertQ, b.Height, positions, hashes, annotatedTxs)
	if err != nil {
		return nil, errors.Wrap(err, "inserting annotated_txs to db")
	}
	return annotatedTxsDecoded, nil
}

func (ind *Indexer) insertAnnotatedOutputs(ctx context.Context, b *bc.Block, annotatedTxs []*AnnotatedTx) error {
	var (
		outputTxPositions pg.Uint32s
		outputIndexes     pg.Uint32s
		outputTxHashes    pq.ByteaArray
		outputData        pq.StringArray
		prevoutHashes     pq.ByteaArray
		prevoutIndexes    pg.Uint32s
	)

	for pos, tx := range b.Transactions {
		for _, in := range tx.Inputs {
			if !in.IsIssuance() {
				outpoint := in.Outpoint()
				prevoutHashes = append(prevoutHashes, outpoint.Hash[:])
				prevoutIndexes = append(prevoutIndexes, outpoint.Index)
			}
		}

		for outIndex, out := range annotatedTxs[pos].Outputs {
			if out.Type == "retire" {
				continue
			}

			outCopy := *out
			outCopy.TransactionID = tx.Hash[:]
			serializedData, err := json.Marshal(outCopy)
			if err != nil {
				return errors.Wrap(err, "serializing annotated output")
			}

			outputTxPositions = append(outputTxPositions, uint32(pos))
			outputIndexes = append(outputIndexes, uint32(outIndex))
			outputTxHashes = append(outputTxHashes, tx.Hash[:])
			outputData = append(outputData, string(serializedData))
		}
	}

	// Insert all of the block's outputs at once.
	const insertQ = `
		INSERT INTO annotated_outputs (block_height, tx_pos, output_index, tx_hash, data, timespan)
		SELECT $1, unnest($2::integer[]), unnest($3::integer[]), unnest($4::bytea[]),
		           unnest($5::jsonb[]),   int8range($6, NULL)
		ON CONFLICT (block_height, tx_pos, output_index) DO NOTHING;
	`
	_, err := ind.db.Exec(ctx, insertQ, b.Height, outputTxPositions,
		outputIndexes, outputTxHashes, outputData, b.TimestampMS)
	if err != nil {
		return errors.Wrap(err, "batch inserting annotated outputs")
	}

	const updateQ = `
		UPDATE annotated_outputs SET timespan = INT8RANGE(LOWER(timespan), $1)
		WHERE (tx_hash, output_index) IN (SELECT unnest($2::bytea[]), unnest($3::integer[]))
	`
	_, err = ind.db.Exec(ctx, updateQ, b.TimestampMS, prevoutHashes, prevoutIndexes)
	return errors.Wrap(err, "updating spent annotated outputs")
}
