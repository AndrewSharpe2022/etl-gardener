package ops

import (
	"context"
	"flag"
	"io"
	"log"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/googleapis/google-cloud-go-testing/bigquery/bqiface"
	"github.com/m-lab/go/dataset"

	"github.com/m-lab/etl-gardener/cloud"
	"github.com/m-lab/etl-gardener/cloud/bq"
	"github.com/m-lab/etl-gardener/state"
	"github.com/m-lab/etl-gardener/tracker"
	"github.com/m-lab/etl/etl"
)

// PartitionedTable creates BQ Table for legacy source templated table
func PartitionedTable(j tracker.Job, ds *dataset.Dataset) bqiface.Table {
	tableName := etl.DirToTablename(j.Datatype)

	src := ds.Table(tableName + "$" + j.Date.Format("20060102"))
	return src
}

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

func isTest() bool {
	return flag.Lookup("test.v") != nil
}

func newStateFunc(state tracker.State) ActionFunc {
	return func(ctx context.Context, tk *tracker.Tracker, j tracker.Job, s tracker.Status) {
		log.Println(j, state)
		err := tk.SetStatus(j, state, "") // TODO support annotation.
		if err != nil {
			log.Println(err)
		}
	}
}

// StandardMonitor creates the standard monitor that handles several state transitions.
// It is currently incomplete.
func StandardMonitor(config cloud.BQConfig, tk *tracker.Tracker) *Monitor {
	m := NewMonitor(config, tk)
	m.AddAction("ParseComplete", tracker.ParseComplete,
		nil,
		newStateFunc(tracker.Stabilizing),
		"Changing to Stabilizing")
	m.AddAction("Stabilizing", tracker.Stabilizing,
		// HACK
		func(ctx context.Context, j tracker.Job) bool { return m.waitForStableTable(ctx, j) == nil },
		newStateFunc(tracker.Deduplicating),
		"Stabilizing")
	m.AddAction("Deduplicating", tracker.Deduplicating,
		// HACK
		nil,
		func(ctx context.Context, tk *tracker.Tracker, j tracker.Job, s tracker.Status) {
			err := m.dedup(ctx, j)
			if err != nil {
				log.Println(err)
				tk.SetJobError(j, err.Error())
				// tracker error
				return
			}
			s.State = tracker.Finishing
			tk.UpdateJob(j, s)
		},
		"Deduplicating")
	return m
}

func (m *Monitor) dedup(ctx context.Context, j tracker.Job) error {
	// Launch the dedup request, and save the JobID
	// Code snippet adapted from dataset.NewDataset
	c, err := bigquery.NewClient(ctx, m.bqconfig.BQProject, m.bqconfig.Options...)
	if err != nil {
		return err
	}
	bqClient := bqiface.AdaptClient(c)
	ds := dataset.Dataset{Dataset: bqClient.Dataset(m.bqconfig.BQBatchDataset), BqClient: bqClient}

	src := TemplateTable(j, &ds)
	dest := PartitionedTable(j, &ds)

	log.Println("Dedupping", src.FullyQualifiedName())
	// TODO move Dedup??
	// TODO - implement backoff?
	bqJob, err := bq.Dedup(ctx, &ds, src.TableID(), dest)
	if err != nil {
		if err == io.EOF {
			if isTest() {
				bqJob, err = ds.BqClient.JobFromID(ctx, "fakeJobID")
				return nil
			}
		} else {
			log.Println(err, src.FullyQualifiedName())
			//t.SetError(ctx, err, "DedupFailed")
			return err
		}
	}
	waitForJob(ctx, bqJob, time.Minute)
	return nil
}

// WaitForJob waits for job to complete.  Uses fibonacci backoff until the backoff
// >= maxBackoff, at which point it continues using same backoff.
// TODO - why don't we just use job.Wait()?  Just because of terminate?
// TODO - develop a BQJob interface for wrapping bigquery.Job, and allowing fakes.
// TODO - move this to go/dataset, since it is bigquery specific and general purpose.
func waitForJob(ctx context.Context, job bqiface.Job, maxBackoff time.Duration) error {
	backoff := 10 * time.Second // Some jobs finish much quicker, but we don't really care that much.
	previous := backoff
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			status, err := job.Wait(ctx)
			if err != nil {
				log.Println(err)
				return err
			} else if status.Err() != nil {
				// NOTE we are getting rate limit exceeded errors here.
				log.Println(job.ID(), status.Err())
				if strings.Contains(status.Err().Error(), "rateLimitExceeded") {
					return state.ErrBQRateLimitExceeded
				}
				if strings.Contains(status.Err().Error(), "Not found: Table") {
					return state.ErrTableNotFound
				}
				if strings.Contains(status.Err().Error(), "rows belong to different partitions") {
					return state.ErrRowsFromOtherPartition
				}
				if backoff == maxBackoff {
					log.Println("reached max backoff")
					// return status.Err()
				}
			} else if status.Done() {
				break
			}
			if backoff+previous < maxBackoff {
				tmp := previous
				previous = backoff
				backoff = backoff + tmp
			} else {
				backoff = maxBackoff
			}

		}
		time.Sleep(backoff)
	}
}
