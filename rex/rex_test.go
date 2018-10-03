package rex_test

import (
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/google-cloud-go-testing/storage/stiface"
	"github.com/m-lab/etl-gardener/cloud"
	"github.com/m-lab/etl-gardener/reproc"
	"github.com/m-lab/etl-gardener/rex"
	"github.com/m-lab/etl-gardener/state"
	"google.golang.org/api/option"
)

func init() {
	// Always prepend the filename and line number.
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
}

type testSaver struct {
	lock   sync.Mutex
	tasks  map[string][]state.Task
	delete map[string]struct{}
}

func newTestSaver() *testSaver {
	return &testSaver{tasks: make(map[string][]state.Task), delete: make(map[string]struct{})}
}

func (s *testSaver) SaveTask(t state.Task) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.tasks[t.Name] = append(s.tasks[t.Name], t)
	return nil
}

func (s *testSaver) DeleteTask(t state.Task) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.delete[t.Name] = struct{}{}
	return nil
}

func (s *testSaver) GetTasks() map[string][]state.Task {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.tasks
}

func (s *testSaver) GetDeletes() map[string]struct{} {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.delete
}

// This test exercises the task management, including invoking t.Process().
// It does not check any state, but if the termination does not work properly,
// may fail to complete.  Also, running with -race may detect race
// conditions.
// TODO - use a fake bigtable, so that tasks can get beyond Stabilizing.
func TestWithTaskQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client, counter := cloud.DryRunClient()
	config := cloud.Config{Project: "mlab-testing", Client: client}
	bqConfig := cloud.BQConfig{Config: config, BQProject: "bqproject", BQDataset: "dataset"}
	bucketOpts := []option.ClientOption{option.WithHTTPClient(client)}
	exec := rex.ReprocessingExecutor{BQConfig: bqConfig, BucketOpts: bucketOpts}
	saver := newTestSaver()
	th := reproc.NewTaskHandler(&exec, []string{"queue-1"}, saver)

	th.AddTask(ctx, "gs://foo/bar/2001/01/01/")

	cancel()
	go th.AddTask(ctx, "gs://foo/bar/2001/01/02/")
	go th.AddTask(ctx, "gs://foo/bar/2001/01/03/")

	start := time.Now()
	for counter.Count() < 3 && time.Now().Before(start.Add(2*time.Second)) {
		time.Sleep(100 * time.Millisecond)
	}

	th.Wait()

	if counter.Count() != 3 {
		t.Error("Wrong number of client calls", counter.Count())
	}
	if len(saver.GetDeletes()) != 3 {
		t.Error("Wrong number of task deletes", len(saver.GetDeletes()))
	}

	tasks := saver.GetTasks()
	for _, task := range tasks {
		log.Println(task)
		if task[len(task)-1].State != state.Done {
			t.Error("Bad task state:", task)
		}
	}

}

// Adapted from google-cloud-go-testing/storage/stiface/stiface_test.go
//
// By using the "interface" version of the client, we make it possible to sub in
// our own fakes at any level. Here we sub in a fake Client which returns a fake
// BucketHandle that returns a fake Object, that returns a Writer in which all
// writes will fail. This is only a "blackbox" test in the most technical of
// senses, but it allows us to exercise error paths.
type fakeClient struct {
	stiface.Client
}

func (f fakeClient) Bucket(name string) stiface.BucketHandle {
	return &fakeBucketHandle{}
}

type fakeBucketHandle struct {
	stiface.BucketHandle
}

func (f fakeBucketHandle) Objects(context.Context, *storage.Query) ObjectIterator {
	return fakeObjectIterator{}
}

type fakeObjectIterator struct {
	stiface.ObjectIterator
	objects []*stiface.ObjectAttrs
	next    int
}

func (it *fakeObjectIterator) Next() (*storage.ObjectAttrs, error) {
	if next >= len(objects) {
		return nil, errors.New("storage error?")
	}
	next++
	return objects[next-1], nil
}
