package rex_test

import (
	"context"
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
	"github.com/m-lab/go/bqext"
	"google.golang.org/api/iterator"
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

// NewReprocessingExecutor creates a new exec.
// NOTE:  The context is used to create a persistent storage Client!
func NewReprocessingExecutor(ctx context.Context, config cloud.BQConfig, bucketOpts []option.ClientOption) (*rex.ReprocessingExecutor, error) {
	// TODO NOW - replace with fake DS
	client, err := bqext.DefaultBQClient(ctx, config.BQProject, bucketOpts...)
	if err != nil {
		return nil, err
	}
	ds := bqext.NewDataset(client, config.BQDataset)
	return &rex.ReprocessingExecutor{BQConfig: config, StorageClient: fakeClient{}, Dataset: ds}, nil
}

// This test exercises the task management, including invoking t.Process().
// It does not check any state, but if the termination does not work properly,
// may fail to complete.  Also, running with -race may detect race
// conditions
// TODO - use a fake bigtable, so that tasks can get beyond Stabilizing.
func TestWithTaskQueue(t *testing.T) {
	ctx := context.Background()
	client, counter := cloud.DryRunClient()
	config := cloud.Config{Project: "mlab-testing", Client: client}
	bqConfig := cloud.BQConfig{Config: config, BQProject: "bqproject", BQDataset: "dataset"}
	bucketOpts := []option.ClientOption{option.WithHTTPClient(client)}
	exec, err := NewReprocessingExecutor(ctx, bqConfig, bucketOpts)
	if err != nil {
		t.Fatal(err)
	}
	saver := newTestSaver()
	th := reproc.NewTaskHandler(exec, []string{"queue-1"}, saver)

	th.AddTask(ctx, "gs://foo/bar/2001/01/01/")

	go th.AddTask(ctx, "gs://foo/bar/2001/01/02/")
	go th.AddTask(ctx, "gs://foo/bar/2001/01/03/")

	start := time.Now()
	for counter.Count() < 9 && time.Now().Before(start.Add(2*time.Second)) {
		time.Sleep(100 * time.Millisecond)
	}

	tasks := saver.GetTasks()
	for _, task := range tasks {
		log.Println(task)
	}

	th.Wait()

	if counter.Count() != 9 {
		t.Error("Wrong number of client calls", counter.Count())
	}
	if len(saver.GetDeletes()) != 3 {
		t.Error("Wrong number of task deletes", len(saver.GetDeletes()))
	}

	tasks = saver.GetTasks()
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

func (f fakeBucketHandle) Objects(context.Context, *storage.Query) stiface.ObjectIterator {
	n := 0
	it := fakeObjectIterator{next: &n}
	it.objects = append(it.objects, &storage.ObjectAttrs{Name: "obj1"})
	it.objects = append(it.objects, &storage.ObjectAttrs{Name: "obj2"})
	return it
}

func (f fakeBucketHandle) Attrs(context.Context) (*storage.BucketAttrs, error) {
	return nil, nil
}

type fakeObjectIterator struct {
	stiface.ObjectIterator
	objects []*storage.ObjectAttrs
	next    *int
}

func (it fakeObjectIterator) Next() (*storage.ObjectAttrs, error) {
	if *it.next >= len(it.objects) {
		return nil, iterator.Done
	}
	log.Println(it.next)
	*it.next += 1
	return it.objects[*it.next-1], nil
}
