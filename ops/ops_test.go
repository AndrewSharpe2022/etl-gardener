// Package ops provides code that observes the tracker state, and takes appropriate actions.
package ops_test

import (
	"context"
	"log"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/googleapis/google-cloud-go-testing/bigquery/bqiface"
	"github.com/m-lab/go/dataset"
	"github.com/m-lab/go/logx"
	"google.golang.org/api/option"

	"github.com/m-lab/etl-gardener/cloud"
	"github.com/m-lab/etl-gardener/ops"
	"github.com/m-lab/etl-gardener/tracker"
	"github.com/m-lab/go/rtx"
)

func init() {
	// Always prepend the filename and line number.
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func newStateFunc(state tracker.State) ops.ActionFunc {
	return func(ctx context.Context, tk *tracker.Tracker, j tracker.Job, s tracker.Status) {
		log.Println(j, state)
		err := tk.SetStatus(j, state, "")
		if err != nil {
			log.Println(err)
		}
	}
}

func TestMonitor_Watch(t *testing.T) {
	logx.LogxDebug.Set("true")

	ctx, cancel := context.WithCancel(context.Background())
	tk, err := tracker.InitTracker(ctx, nil, nil, 0, 0)
	rtx.Must(err, "tk init")
	tk.AddJob(tracker.NewJob("bucket", "exp", "type", time.Now()))
	tk.AddJob(tracker.NewJob("bucket", "exp2", "type", time.Now()))
	tk.AddJob(tracker.NewJob("bucket", "exp2", "type2", time.Now()))

	m := ops.NewMonitor(cloud.BQConfig{}, tk)
	m.AddAction("Init", tracker.Init,
		nil,
		newStateFunc(tracker.Parsing),
		"Init")
	m.AddAction("Parsing", tracker.Parsing,
		nil,
		newStateFunc(tracker.ParseComplete),
		"Parsing")
	m.AddAction("ParseComplete", tracker.ParseComplete,
		nil,
		newStateFunc(tracker.Stabilizing),
		"PostProcessing")
	m.AddAction("Stabilizing", tracker.Stabilizing,
		nil,
		newStateFunc(tracker.Deduplicating),
		"Checking for stability")
	m.AddAction("Deduplicating", tracker.Deduplicating,
		nil,
		newStateFunc(tracker.Complete),
		"Deduplicating")
	go m.Watch(ctx, 10*time.Millisecond)

	failTime := time.Now().Add(10 * time.Second)

	for time.Now().Before(failTime) && tk.NumJobs() > 0 {
	}
	if tk.NumJobs() != 0 {
		t.Error(tk.NumJobs())
	}
	cancel()
}

func TestMonitor_Tables(t *testing.T) {
	ctx := context.Background()

	project := "fakeProject"
	dryRun, _ := cloud.DryRunClient()
	config := cloud.Config{
		Project: project,
		Client:  http.DefaultClient,
		Options: []option.ClientOption{option.WithHTTPClient(dryRun)},
	}
	bqConfig := cloud.BQConfig{Config: config, BQFinalDataset: "final", BQBatchDataset: "batch"}

	c, err := bigquery.NewClient(ctx, bqConfig.BQProject, bqConfig.Options...)
	rtx.Must(err, "client")
	bqClient := bqiface.AdaptClient(c)
	ds := dataset.Dataset{Dataset: bqClient.Dataset(bqConfig.BQBatchDataset), BqClient: bqClient}

	j := tracker.NewJob("bucket", "exp", "type", time.Date(2019, 12, 1, 0, 0, 0, 0, time.UTC))
	src := ops.TemplateTable(j, &ds)
	dest := ops.PartitionedTable(j, &ds)

	if src.DatasetID() != "batch" {
		t.Error(src)
	}
	if dest.DatasetID() != "batch" {
		t.Error(src)
	}

}
