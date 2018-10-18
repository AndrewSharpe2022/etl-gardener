package bq_test

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/m-lab/etl-gardener/cloud/bq"
	"github.com/m-lab/go/cloud/bqfake"
)

func fakeMeta(partitioned bool) bigquery.TableMetadata {
	meta := bigquery.TableMetadata{CreationTime: time.Now(), LastModifiedTime: time.Now(), NumBytes: 168, NumRows: 8}
	if partitioned {
		meta.TimePartitioning = &bigquery.TimePartitioning{Expiration: 0 * time.Second}
	}
	return meta
}

func TestWaitForStableTable(t *testing.T) {
	ctx := context.Background()
	prj := "fakeProject"
	dsName := "fakeDataset"
	c, err := bqfake.NewClient(ctx, prj)
	if err != nil {
		t.Fatal(err)
	}

	ds := c.Dataset(dsName)
	tbl := ds.Table("fakeTable")

	// First try with non-existant table
	err = bq.WaitForStableTable(ctx, tbl)
	if err == nil {
		t.Error("Should have generated error")
	} else if err.Error() != "Table not found" {
		t.Error("Should be table not found:", err)
	}

	meta := fakeMeta(true)
	// TODO: once we have either mock ability to update StreamingBuffer,
	// or fake support for StreamingBuffer simulation, we should
	// add that to this test.
	// TODO meta.StreamingBuffer = &bigquery.StreamingBuffer{}
	err = tbl.Create(ctx, &meta)
	if err != nil {
		t.Error(err)
	}

	bq.WaitForStableTable(ctx, tbl)
}
