// Package etcdqueue implements queue service backed by etcd.
package etcdqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/golang/glog"
)

const (
	// MaxWeight is the maximum value for item(job) weights.
	MaxWeight uint64 = 99999

	// MaxProgress is the progress value when the job is done!
	MaxProgress = 100
)

// Item represents a job item in the queue. Key is stored as a key,
// with serialized JSON data as a value.
type Item struct {
	// Bucket is the name or job category for namespacing.
	// All keys will be prefixed with bucket name.
	Bucket string `json:"bucket"`

	// CreatedAt is timestamp of item creation.
	CreatedAt time.Time `json:"created_at"`

	// Key is autogenerated based on timestamps and bucket name.
	// It is stored as a key in etcd.
	Key string `json:"key"`

	// Value contains any data (e.g. encoded computation results).
	Value string `json:"value"`

	// Progress is the progress status value (range from 0 to 'etcdqueue.MaxProgress').
	Progress int `json:"progress"`

	// Canceled is true if the item(or job) is canceled.
	Canceled bool `json:"canceled"`

	// Error contains any error message. It's defined as string for
	// different language interpolation.
	Error string `json:"error"`

	// RequestID is used/generated by external service,
	// to help identify each item.
	RequestID string `json:"request_id"`
}

// CreateItem creates an item with auto-generated ID of unix nano seconds.
// The maximum weight(priority) is 99999.
func CreateItem(bucket string, weight uint64, value string) *Item {
	if weight > MaxWeight {
		weight = MaxWeight
	}
	// maximum weight comes first, lexicographically
	priority := 99999 - weight

	createdAt := time.Now()
	key := path.Join(bucket, fmt.Sprintf("%05d%035X", priority, createdAt.UnixNano()))

	return &Item{
		Bucket:    bucket,
		CreatedAt: createdAt,
		Key:       key,
		Value:     value,
		Progress:  0,
		Error:     "",
	}
}

// Equal compares two items with truncated CreatedAt field string,
// to handle modified timestamp string after serialization
func (item1 *Item) Equal(item2 *Item) error {
	if item1.CreatedAt.String()[:29] != item2.CreatedAt.String()[:29] {
		return fmt.Errorf("expected CreatedAt %q, got %q", item1.CreatedAt.String()[:29], item2.CreatedAt.String()[:29])
	}
	if item1.Bucket != item2.Bucket {
		return fmt.Errorf("expected Bucket %q, got %q", item1.Bucket, item2.Bucket)
	}
	if item1.Key != item2.Key {
		return fmt.Errorf("expected Key %q, got %q", item1.Key, item2.Key)
	}
	if item1.Value != item2.Value {
		return fmt.Errorf("expected Value %q, got %q", item1.Value, item2.Value)
	}
	if item1.Progress != item2.Progress {
		return fmt.Errorf("expected Progress %d, got %d", item1.Progress, item2.Progress)
	}
	if item1.Canceled != item2.Canceled {
		return fmt.Errorf("expected Canceled %v, got %v", item1.Canceled, item2.Canceled)
	}
	if item1.Error != item2.Error {
		return fmt.Errorf("expected Error %s, got %s", item1.Error, item2.Error)
	}
	if item1.RequestID != item2.RequestID {
		return fmt.Errorf("expected RequestID %s, got %s", item1.RequestID, item2.RequestID)
	}
	return nil
}

// ItemWatcher is receive-only channel, used for broadcasting status updates.
type ItemWatcher <-chan *Item

// Op represents an operation that queue can execute.
type Op struct {
	ttl int64
}

// OpOption configures queue operations.
type OpOption func(*Op)

// WithTTL configures TTL.
func WithTTL(dur time.Duration) OpOption {
	return func(op *Op) { op.ttl = int64(dur.Seconds()) }
}

func (op *Op) applyOpts(opts []OpOption) {
	for _, opt := range opts {
		opt(op)
	}
}

// Queue is the queue service backed by etcd.
type Queue interface {
	// Add adds an item to the queue.
	Add(ctx context.Context, it *Item, opts ...OpOption) error

	// Pop returns ItemWatcher that returns the first item in the queue.
	// It blocks until there is at least one item to return.
	Pop(ctx context.Context, bucket string) ItemWatcher

	// Stop stops the queue service and any embedded clients.
	Stop()

	// Client returns the client.
	Client() *clientv3.Client

	// ClientEndpoints returns the client endpoints.
	ClientEndpoints() []string
}

type queue struct {
	writemu    sync.RWMutex
	cli        *clientv3.Client
	rootCtx    context.Context
	rootCancel func()
}

// NewQueue creates a new queue from given etcd client.
func NewQueue(cli *clientv3.Client) (Queue, error) {
	// issue linearized read to ensure leader election
	glog.Infof("GET request to endpoint %v", cli.Endpoints())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := cli.Get(ctx, "foo")
	cancel()
	glog.Infof("GET request succeeded on endpoint %v", cli.Endpoints())
	if err != nil {
		return nil, err
	}

	ctx, cancel = context.WithCancel(context.Background())
	return &queue{
		cli:        cli,
		rootCtx:    ctx,
		rootCancel: cancel,
	}, nil
}

const pfxQueue = "_queue"

func (qu *queue) Add(ctx context.Context, item *Item, opts ...OpOption) error {
	if item == nil {
		return fmt.Errorf("received <nil> Item")
	}

	ret := Op{}
	ret.applyOpts(opts)

	queueKey := path.Join(pfxQueue, item.Key)
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	queueVal := string(data)

	qu.writemu.Lock()
	defer qu.writemu.Unlock()

	if err := qu.put(ctx, queueKey, queueVal, ret.ttl); err != nil {
		return err
	}
	glog.Infof("queue: wrote %q with TTL %d", item.Key, ret.ttl)
	return nil
}

func (qu *queue) Pop(ctx context.Context, bucket string) ItemWatcher {
	ch := make(chan *Item, 1)

	pfxQueueBucket := path.Join(pfxQueue, bucket)
	resp, err := qu.cli.Get(ctx, pfxQueueBucket, clientv3.WithFirstKey()...)
	if err != nil {
		ch <- &Item{Error: err.Error()}
		close(ch)
		return ch
	}

	if len(resp.Kvs) == 1 {
		v := resp.Kvs[0].Value
		var item Item
		if err = json.Unmarshal(v, &item); err != nil {
			ch <- &Item{Error: fmt.Sprintf("%q returned wrong JSON %q (%v)", pfxQueueBucket, string(v), err)}
			close(ch)
			return ch
		}

		queueKey := path.Join(pfxQueue, item.Key)
		if _, err = qu.cli.Delete(ctx, queueKey); err != nil {
			ch <- &Item{Error: fmt.Sprintf("failed to delete %q (%v)", queueKey, err)}
			close(ch)
			return ch
		}

		ch <- &item
		close(ch)
		return ch
	}

	if len(resp.Kvs) == 0 {
		wch := qu.cli.Watch(ctx, pfxQueueBucket, clientv3.WithPrefix(), clientv3.WithCreatedNotify())
		if _, ok := <-wch; !ok {
			ch <- &Item{Error: fmt.Sprintf("watch failed to create %q (%v)", pfxQueueBucket, err)}
			close(ch)
			return ch
		}

		go func() {
			defer close(ch)

			select {
			case wresp := <-wch:
				if len(wresp.Events) != 1 {
					ch <- &Item{Error: fmt.Sprintf("%q did not return 1 event via watch (got %+v)", pfxQueueBucket, wresp)}
					return
				}
				if wresp.Err() != nil {
					ch <- &Item{Error: fmt.Sprintf("%q returned error %v", pfxQueueBucket, wresp.Err())}
					return
				}
				if wresp.Canceled || wresp.Events[0].Type == mvccpb.DELETE {
					ch <- &Item{Error: fmt.Sprintf("%q watch has been canceled or deleted", pfxQueueBucket)}
					return
				}

				v := wresp.Events[0].Kv.Value
				var item Item
				if err := json.Unmarshal(v, &item); err != nil {
					ch <- &Item{Error: fmt.Sprintf("%q returned wrong JSON value %q (%v)", pfxQueueBucket, string(v), err)}
					return
				}

				queueKey := path.Join(pfxQueue, item.Key)
				if _, err := qu.cli.Delete(ctx, queueKey); err != nil {
					ch <- &Item{Error: fmt.Sprintf("failed to delete %q (%v)", queueKey, err)}
					return
				}
				ch <- &item

			case <-ctx.Done():
				ch <- &Item{Error: ctx.Err().Error()}
			}
		}()
		return ch
	}

	ch <- &Item{Error: fmt.Sprintf("%q returned more than 1 key", pfxQueueBucket)}
	close(ch)
	return ch
}

func (qu *queue) Stop() {
	qu.writemu.Lock()
	defer qu.writemu.Unlock()

	glog.Info("stopping queue")
	qu.rootCancel()
	qu.cli.Close()
	glog.Info("stopped queue")
}

func (qu *queue) Client() *clientv3.Client {
	return qu.cli
}

func (qu *queue) ClientEndpoints() []string {
	return qu.cli.Endpoints()
}

func (qu *queue) put(ctx context.Context, key, val string, ttl int64) error {
	var opts []clientv3.OpOption
	if ttl > 5 {
		resp, err := qu.cli.Grant(ctx, ttl)
		if err != nil {
			return err
		}
		leaseID := resp.ID
		opts = append(opts, clientv3.WithLease(leaseID))
	}
	_, err := qu.cli.Put(ctx, key, val, opts...)
	return err
}

func (qu *queue) delete(ctx context.Context, key string) error {
	_, err := qu.cli.Delete(ctx, key)
	return err
}
