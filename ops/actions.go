package ops

import (
	"context"
	"log"

	"cloud.google.com/go/bigquery"
	"github.com/GoogleCloudPlatform/google-cloud-go-testing/bigquery/bqiface"
	"github.com/m-lab/etl-gardener/cloud"
	"github.com/m-lab/etl-gardener/cloud/bq"
	"github.com/m-lab/etl-gardener/state"
	"github.com/m-lab/etl-gardener/tracker"
	"github.com/m-lab/etl/etl"
	"github.com/m-lab/go/dataset"
)

// TemplateTable creates BQ Table for legacy source templated table
func TemplateTable(j tracker.Job, ds *dataset.Dataset) bqiface.Table {
	tableName := etl.DirToTablename(j.Datatype)

	src := ds.Table(tableName + "_" + j.Date.Format("20060102"))
	return src
}

// This is a function I didn't want to port to the new architecture.  8-(
func (m *Monitor) waitForStableTable(ctx context.Context, j tracker.Job) error {
	log.Println("Stabilizing:", j)
	// Wait for the streaming buffer to be nil.

	// Code snippet adapted from dataset.NewDataset
	c, err := bigquery.NewClient(ctx, m.bqconfig.BQProject, m.bqconfig.Options...)
	if err != nil {
		return err
	}
	bqClient := bqiface.AdaptClient(c)
	ds := dataset.Dataset{Dataset: bqClient.Dataset(m.bqconfig.BQBatchDataset), BqClient: bqClient}

	src := TemplateTable(j, &ds)
	err = bq.WaitForStableTable(ctx, src)
	if err != nil {
		// When testing, we expect to get ErrTableNotFound here.
		if err != state.ErrTableNotFound {
			// t.SetError(ctx, err, "bq.WaitForStableTable")
			return err
		}
	}

	return nil
}

// StandardMonitor creates the standard monitor that handles several state transitions.
// It is currently incomplete.
func StandardMonitor(config cloud.BQConfig) *Monitor {
	m := NewMonitor(config)
	m.AddAction(tracker.ParseComplete,
		trueCondition,
		newStateFunc(tracker.Stabilizing),
		"Stabilizing")
	m.AddAction(tracker.Stabilizing,
		// HACK
		func(ctx context.Context, j tracker.Job) bool { return m.waitForStableTable(ctx, j) == nil },
		newStateFunc(tracker.Deduplicating),
		"Deduplicating not implemented")
	return m
}
