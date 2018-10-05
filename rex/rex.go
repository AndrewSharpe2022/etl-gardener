// Package rex provides ReprocessingExecutor, which implements the state.Executor interface.
// It provides all the implementation detail for reprocessing data through the etl pipeline.
package rex

// Design Notes:
// 1. Tasks contain all info about queues in use.
// 2. Tasks in flight are not cached locally, as they are available from Datastore.

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"math/rand"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/google-cloud-go-testing/bigquery/bqiface"
	"github.com/GoogleCloudPlatform/google-cloud-go-testing/storage/stiface"
	"github.com/m-lab/etl-gardener/cloud"
	"github.com/m-lab/etl-gardener/cloud/bq"
	"github.com/m-lab/etl-gardener/cloud/tq"
	"github.com/m-lab/etl-gardener/metrics"
	"github.com/m-lab/etl-gardener/state"
	"github.com/m-lab/go/bqext"
	"google.golang.org/api/option"
)

// Environment provides "global" variables.
type environment struct {
	TestMode bool
}

// env provides environment vars.
var env environment

func init() {
	// HACK This allows some modified behavior when running unit tests.
	if flag.Lookup("test.v") != nil {
		env.TestMode = true
	}
}

// ReprocessingExecutor handles all reprocessing steps.
type ReprocessingExecutor struct {
	cloud.BQConfig
	StorageClient stiface.Client
	Dataset       bqext.Dataset
}

// NewReprocessingExecutor creates a new exec.
// NOTE:  The context is used to create a persistent storage Client!
func NewReprocessingExecutor(ctx context.Context, config cloud.BQConfig, bucketOpts []option.ClientOption) (*ReprocessingExecutor, error) {
	storageClient, err := storage.NewClient(ctx, bucketOpts...)
	if err != nil {
		return nil, err
	}
	client, err := bqext.DefaultBQClient(ctx, config.BQProject, bucketOpts...)
	if err != nil {
		return nil, err
	}
	ds := bqext.NewDataset(client, config.BQDataset)
	return &ReprocessingExecutor{config, stiface.AdaptClient(storageClient), ds}, nil
}

// GetDS constructs an appropriate Dataset for BQ operations.
// TODO NOW remove this.
func (rex *ReprocessingExecutor) GetDS() (bqext.Dataset, error) {
	return rex.Dataset, nil
}

// ErrNoSaver is returned when saver has not been set.
var ErrNoSaver = errors.New("Task.saver is nil")

// Next executes the currently specified action, updates Err/ErrInfo, and advances
// the state.
// It may terminate prematurely if terminate is closed prior to completion.
// TODO - consider returning any error to caller.
func (rex *ReprocessingExecutor) Next(ctx context.Context, t *state.Task, terminate <-chan struct{}) error {
	// Note that only the state in state.Task is maintained between actions.
	switch t.State {
	case state.Initializing:
		// TODO - should check that _suffix table doesn't already exist, or delete it
		// if it does?  Also check if it has a streaming buffer?
		// Nothing to do.
		t.Update(state.Queuing)
		metrics.StartedCount.WithLabelValues("sidestream").Inc()

	case state.Queuing:
		// TODO - handle zero task case.
		n, err := rex.queue(ctx, t)
		if err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "rex.queue")
			return err
		}
		if n < 1 {
			// If there were no tasks posted, then there is nothing more to do.
			t.Queue = ""
			t.Update(state.Done)
		} else {
			t.Update(state.Processing)
		}

	case state.Processing: // TODO should this be Parsing?
		if err := rex.waitForParsing(t, terminate); err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "rex.waitForParsing")
			return err
		}
		t.Queue = "" // No longer need to keep the queue.
		t.Update(state.Stabilizing)

	case state.Stabilizing:
		// Wait for the streaming buffer to be nil.
		ds, err := rex.GetDS()
		if err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "rex.GetDS")
			return err
		}
		s, _, err := t.SourceAndDest(&ds)
		if err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "task.SourceAndDest")
			return err
		}
		err = bq.WaitForStableTable(ctx, s)
		if err != nil {
			// When testing, we expect to get ErrTableNotFound here.
			if !env.TestMode || err != state.ErrTableNotFound {
				// SetError also pushes to datastore, like Update()
				t.SetError(err, "bq.WaitForStableTable")
				return err
			}
		}

		t.Update(state.Deduplicating)

	case state.Deduplicating:
		if err := rex.dedup(ctx, t); err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "rex.dedup")
			return err
		}
		t.Update(state.Finishing)

	case state.Finishing:
		log.Println("Finishing")
		if err := rex.finish(ctx, t, terminate); err != nil {
			// SetError also pushes to datastore, like Update()
			t.SetError(err, "rex.finish")
			return err
		}
		t.JobID = ""
		t.Update(state.Done)

	case state.Done:
		log.Println("Unexpected call to Next when Done")

	case state.Invalid:
		log.Println("Should not call Next on Invalid state!")
		err := errors.New("called Next on invalid state")
		// SetError also pushes to datastore, like Update()
		t.SetError(err, "Invalid state")
		return err

	default:
		log.Println("Unknown state")
		err := errors.New("Unknown state")
		// SetError also pushes to datastore, like Update()
		t.SetError(err, "Unknown state")
		return err
	}

	return nil
}

// TODO should these take Task instead of *Task?
func (rex *ReprocessingExecutor) waitForParsing(t *state.Task, terminate <-chan struct{}) error {
	// Wait for the queue to drain.
	// Don't want to accept a date until we can actually queue it.
	qh, err := tq.NewQueueHandler(rex.Config, t.Queue)
	if err != nil {
		t.SetError(err, "NewQueueHandler")
		return err
	}
	log.Println("Wait for empty queue ", qh.Queue)
	for err := qh.IsEmpty(); err != nil; err = qh.IsEmpty() {
		select {
		case <-terminate:
			t.SetError(err, "Terminating")
			return err
		default:
		}
		if err == tq.ErrMoreTasks {
			// Wait 5-15 seconds before checking again.
			time.Sleep(time.Duration(5+rand.Intn(10)) * time.Second)
		} else if err != nil {
			if err == io.EOF && env.TestMode {
				// Expected when using test client.
				return nil
			}
			// We don't expect errors here, so try logging, and a large backoff
			// in case there is some bad network condition, service failure,
			// or perhaps the queue_pusher is down.
			log.Println(err)
			metrics.WarningCount.WithLabelValues("IsEmptyError").Inc()
			// TODO update metric
			time.Sleep(time.Duration(60+rand.Intn(120)) * time.Second)
		}
	}
	return nil
}

func (rex *ReprocessingExecutor) queue(ctx context.Context, t *state.Task) (int, error) {
	// Submit all files from the bucket that match the prefix.
	// Where do we get the bucket?
	//func (qh *ChannelQueueHandler) handleLoop(next api.BasicPipe, bucketOpts ...option.ClientOption) {
	qh, err := tq.NewQueueHandler(rex.Config, t.Queue)
	if err != nil {
		t.SetError(err, "NewQueueHandler")
		return 0, err
	}
	parts, err := t.ParsePrefix()
	if err != nil {
		// If there is a parse error, log and skip request.
		log.Println(err)
		t.SetError(err, "BadPrefix")
		return 0, err
	}
	bucketName := parts[0]

	// Use a real storage bucket.
	// TODO - try cancelling the context instead?
	bucket, err := tq.GetBucket(ctx, rex.StorageClient, rex.Project, bucketName, false)
	if err != nil {
		if err == io.EOF && env.TestMode {
			log.Println("Using fake client, ignoring EOF error")
			return 0, nil
		}
		log.Println(err)
		t.SetError(err, "BucketError")
		return 0, err
	}
	// NOTE: This does not check the terminate channel, so once started, it will
	// complete the queuing.
	n, err := qh.PostDay(ctx, bucket, bucketName, parts[1]+"/"+parts[2]+"/")
	if err != nil {
		log.Println(err)
		t.SetError(err, "PostDayError")
		return n, nil
	}
	log.Println("Added ", n, t.Name, " tasks to ", qh.Queue)
	return n, nil
}

func (rex *ReprocessingExecutor) dedup(ctx context.Context, t *state.Task) error {
	// Launch the dedup request, and save the JobID
	ds, err := rex.GetDS()
	if err != nil {
		t.SetError(err, "GetDS")
		return err
	}
	src, dest, err := t.SourceAndDest(&ds)
	if err != nil {
		t.SetError(err, "SourceAndDest")
		return err
	}

	log.Println("Dedupping", src.FullyQualifiedName())
	// TODO move Dedup??
	job, err := bq.Dedup(ctx, &ds, src.TableID(), dest)
	if err != nil {
		if err == io.EOF {
			if env.TestMode {
				t.JobID = "fakeJobID"
				return nil
			}
		} else {
			log.Println(err, src.FullyQualifiedName())
			t.SetError(err, "DedupFailed")
			return err
		}
	}
	t.JobID = job.ID()
	return nil
}

// WaitForJob waits for job to complete.  Uses fibonacci backoff until the backoff
// >= maxBackoff, at which point it continues using same backoff.
// TODO - develop a BQJob interface for wrapping bigquery.Job, and allowing fakes.
// TODO - move this to go/bqext, since it is bigquery specific and general purpose.
func waitForJob(ctx context.Context, job bqiface.Job, maxBackoff time.Duration, terminate <-chan struct{}) error {
	backoff := 10 * time.Millisecond
	previous := backoff
	for {
		select {
		case <-terminate:
			return state.ErrTaskSuspended
		default:
		}
		status, err := job.Status(ctx)
		if err != nil {
			log.Println(err)
		} else if status.Err() != nil {
			log.Println(job.ID(), status.Err())
			if strings.Contains(status.Err().Error(), "Not found: Table") {
				return state.ErrTableNotFound
			}
			if strings.Contains(status.Err().Error(), "rows belong to different partitions") {
				return state.ErrRowsFromOtherPartition
			}
			if backoff == maxBackoff {
				return status.Err()
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

		time.Sleep(backoff)
	}
	return nil
}

func (rex *ReprocessingExecutor) finish(ctx context.Context, t *state.Task, terminate <-chan struct{}) error {
	// TODO use a simple client instead of creating dataset?
	client, err := bqext.DefaultBQClient(ctx, rex.Project, rex.Options...)
	if err != nil {
		return err
	}
	ds := bqext.NewDataset(client, rex.BQDataset)
	if err != nil {
		t.SetError(err, "NewDataset")
		return err
	}
	src, _, err := t.SourceAndDest(&ds)
	if err != nil {
		t.SetError(err, "SourceAndDest")
		return err
	}
	job, err := ds.BqClient.JobFromID(ctx, t.JobID)
	if err != nil {
		t.SetError(err, "JobFromID")
		return err
	}
	// TODO - should loop, and check terminate channel
	err = waitForJob(ctx, job, 60*time.Second, terminate)
	if err != nil {
		log.Println(err, src.FullyQualifiedName())
		t.SetError(err, "waitForJob")
		return err
	}
	// TODO - should this context have a deadline?
	status, err := job.Wait(ctx)
	if err != nil {
		if err != state.ErrTaskSuspended {
			log.Println(status.Err(), src.FullyQualifiedName())
			t.SetError(err, "job.Wait")
		}
		return err
	}

	// Wait for JobID to complete, then delete the template table.
	log.Println("Completed deduplication, deleting", src.FullyQualifiedName())
	// If deduplication was successful, we should delete the source table.
	ctx, cf := context.WithTimeout(ctx, time.Minute)
	defer cf()
	err = src.Delete(ctx)
	if err != nil {
		log.Println(err)
		t.SetError(err, "TableDelete")
		return err
	}

	// TODO - Copy to base_tables.
	return nil
}
