package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrQueueTimeout = errors.New("inference queue wait timeout")

type QueueConfig struct {
	GlobalLimit       int
	PerPrincipalLimit int
	WorkloadLimits    map[Workload]int
	LeaseTTL          time.Duration
	MaxWait           time.Duration
	PollInterval      time.Duration
}

type Queue struct {
	redis *redis.Client
	cfg   QueueConfig
}

type Ticket struct {
	RequestID  string    `json:"requestId"`
	Principal  string    `json:"principal"`
	Workload   Workload  `json:"workload"`
	Priority   int       `json:"priority"`
	EnqueuedAt time.Time `json:"enqueuedAt"`
	AcquiredAt time.Time `json:"acquiredAt"`
	q          *Queue
	done       chan struct{}
	once       sync.Once
}

type QueueSnapshot struct {
	GlobalActive int64              `json:"globalActive"`
	Queued       map[Workload]int64 `json:"queued"`
	Active       map[Workload]int64 `json:"active"`
}

func NewQueue(client *redis.Client, cfg QueueConfig) *Queue {
	if cfg.GlobalLimit <= 0 {
		cfg.GlobalLimit = 4
	}
	if cfg.PerPrincipalLimit <= 0 {
		cfg.PerPrincipalLimit = 1
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 2 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.WorkloadLimits == nil {
		cfg.WorkloadLimits = map[Workload]int{WorkloadFast: 4, WorkloadDeep: 1, WorkloadVision: 1, WorkloadImage: 1, WorkloadEmbedding: 2, WorkloadBatch: 1}
	}
	return &Queue{redis: client, cfg: cfg}
}

func (q *Queue) Acquire(ctx context.Context, requestID, principal string, workload Workload, priority int) (*Ticket, error) {
	if q == nil || q.redis == nil {
		return nil, errors.New("queue unavailable")
	}
	if requestID == "" || principal == "" {
		return nil, errors.New("invalid queue identity")
	}
	if priority < 0 {
		priority = 0
	}
	if priority > 9 {
		priority = 9
	}
	now := time.Now().UTC()
	score := float64((9-priority))*1e13 + float64(now.UnixMilli())
	queueKey := q.queueKey(workload)
	principalHash := queueKey + ":principal"
	expiryHash := queueKey + ":expiry"
	expiresAt := now.Add(q.cfg.MaxWait).UnixMilli()
	pipe := q.redis.TxPipeline()
	pipe.ZAdd(ctx, queueKey, redis.Z{Score: score, Member: requestID})
	pipe.HSet(ctx, principalHash, requestID, principal)
	pipe.HSet(ctx, expiryHash, requestID, expiresAt)
	pipe.Expire(ctx, queueKey, q.cfg.MaxWait+time.Minute)
	pipe.Expire(ctx, principalHash, q.cfg.MaxWait+time.Minute)
	pipe.Expire(ctx, expiryHash, q.cfg.MaxWait+time.Minute)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, q.cfg.MaxWait)
	defer cancel()
	ticker := time.NewTicker(q.cfg.PollInterval)
	defer ticker.Stop()
	for {
		ok, err := q.tryAcquire(waitCtx, requestID, principal, workload)
		if err != nil {
			q.cancel(context.Background(), requestID, workload)
			return nil, err
		}
		if ok {
			t := &Ticket{RequestID: requestID, Principal: principal, Workload: workload, Priority: priority, EnqueuedAt: now, AcquiredAt: time.Now().UTC(), q: q, done: make(chan struct{})}
			go t.renew(ctx)
			return t, nil
		}
		select {
		case <-waitCtx.Done():
			q.cancel(context.Background(), requestID, workload)
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrQueueTimeout
			}
			return nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (q *Queue) tryAcquire(ctx context.Context, requestID, principal string, workload Workload) (bool, error) {
	limit := q.cfg.WorkloadLimits[workload]
	if limit <= 0 {
		limit = 1
	}
	ttl := q.cfg.LeaseTTL.Milliseconds()
	now := time.Now().UnixMilli()
	expiry := now + ttl
	script := redis.NewScript(`
local now=tonumber(ARGV[1]); local expiry=tonumber(ARGV[2]); local globalLimit=tonumber(ARGV[3]); local workloadLimit=tonumber(ARGV[4]); local request=ARGV[5]; local principal=ARGV[6]; local ttl=tonumber(ARGV[7]);
redis.call('ZREMRANGEBYSCORE',KEYS[4],'-inf',now); redis.call('ZREMRANGEBYSCORE',KEYS[5],'-inf',now);
if redis.call('ZCARD',KEYS[4])>=globalLimit or redis.call('ZCARD',KEYS[5])>=workloadLimit then return 0 end;
local candidates=redis.call('ZRANGE',KEYS[1],0,63); local selected=nil;
for _,m in ipairs(candidates) do
  local exp=tonumber(redis.call('HGET',KEYS[3],m) or '0');
  if exp>0 and exp<now then redis.call('ZREM',KEYS[1],m);redis.call('HDEL',KEYS[2],m);redis.call('HDEL',KEYS[3],m);
  else
    local p=redis.call('HGET',KEYS[2],m);
    if p and redis.call('EXISTS','daiki:queue:principal:'..p)==0 then selected=m;break end;
  end
end
if selected~=request then return 0 end;
local principalKey='daiki:queue:principal:'..principal;
if not redis.call('SET',principalKey,request,'PX',ttl,'NX') then return 0 end;
redis.call('ZREM',KEYS[1],request);redis.call('HDEL',KEYS[2],request);redis.call('HDEL',KEYS[3],request);
redis.call('ZADD',KEYS[4],expiry,request);redis.call('ZADD',KEYS[5],expiry,request);redis.call('HSET',KEYS[6],request,principal);redis.call('EXPIRE',KEYS[6],math.ceil(ttl/1000)+60);return 1`)
	v, err := script.Run(ctx, q.redis, []string{q.queueKey(workload), q.queueKey(workload) + ":principal", q.queueKey(workload) + ":expiry", q.globalLeaseKey(), q.workloadLeaseKey(workload), q.leasePrincipalHash()}, now, expiry, q.cfg.GlobalLimit, limit, requestID, principal, ttl).Int()
	return v == 1, err
}

func (t *Ticket) renew(ctx context.Context) {
	tick := time.NewTicker(t.q.cfg.LeaseTTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.done:
			return
		case <-tick.C:
			_ = t.q.renew(context.Background(), t)
		}
	}
}
func (q *Queue) renew(ctx context.Context, t *Ticket) error {
	ttl := q.cfg.LeaseTTL.Milliseconds()
	expiry := time.Now().Add(q.cfg.LeaseTTL).UnixMilli()
	script := redis.NewScript(`local pk='daiki:queue:principal:'..ARGV[1];if redis.call('GET',pk)~=ARGV[2] then return 0 end;redis.call('PEXPIRE',pk,ARGV[3]);redis.call('ZADD',KEYS[1],ARGV[4],ARGV[2]);redis.call('ZADD',KEYS[2],ARGV[4],ARGV[2]);return 1`)
	_, err := script.Run(ctx, q.redis, []string{q.globalLeaseKey(), q.workloadLeaseKey(t.Workload)}, t.Principal, t.RequestID, ttl, expiry).Result()
	return err
}
func (t *Ticket) Release(ctx context.Context) {
	if t == nil || t.q == nil {
		return
	}
	t.once.Do(func() { close(t.done); t.q.release(ctx, t) })
}
func (q *Queue) release(ctx context.Context, t *Ticket) {
	script := redis.NewScript(`local pk='daiki:queue:principal:'..ARGV[1];if redis.call('GET',pk)==ARGV[2] then redis.call('DEL',pk) end;redis.call('ZREM',KEYS[1],ARGV[2]);redis.call('ZREM',KEYS[2],ARGV[2]);redis.call('HDEL',KEYS[3],ARGV[2]);return 1`)
	_, _ = script.Run(ctx, q.redis, []string{q.globalLeaseKey(), q.workloadLeaseKey(t.Workload), q.leasePrincipalHash()}, t.Principal, t.RequestID).Result()
}
func (q *Queue) cancel(ctx context.Context, requestID string, workload Workload) {
	pipe := q.redis.TxPipeline()
	pipe.ZRem(ctx, q.queueKey(workload), requestID)
	pipe.HDel(ctx, q.queueKey(workload)+":principal", requestID)
	pipe.HDel(ctx, q.queueKey(workload)+":expiry", requestID)
	_, _ = pipe.Exec(ctx)
}
func (q *Queue) Snapshot(ctx context.Context) (QueueSnapshot, error) {
	out := QueueSnapshot{Queued: map[Workload]int64{}, Active: map[Workload]int64{}}
	now := time.Now().UnixMilli()
	_ = q.redis.ZRemRangeByScore(ctx, q.globalLeaseKey(), "-inf", fmt.Sprint(now)).Err()
	var err error
	out.GlobalActive, err = q.redis.ZCard(ctx, q.globalLeaseKey()).Result()
	if err != nil {
		return out, err
	}
	for _, w := range []Workload{WorkloadFast, WorkloadDeep, WorkloadVision, WorkloadImage, WorkloadEmbedding, WorkloadBatch} {
		out.Queued[w], _ = q.redis.ZCard(ctx, q.queueKey(w)).Result()
		_ = q.redis.ZRemRangeByScore(ctx, q.workloadLeaseKey(w), "-inf", fmt.Sprint(now)).Err()
		out.Active[w], _ = q.redis.ZCard(ctx, q.workloadLeaseKey(w)).Result()
	}
	return out, nil
}
func (q *Queue) queueKey(w Workload) string         { return "daiki:queue:" + string(w) }
func (q *Queue) globalLeaseKey() string             { return "daiki:queue:leases:global" }
func (q *Queue) workloadLeaseKey(w Workload) string { return "daiki:queue:leases:" + string(w) }
func (q *Queue) leasePrincipalHash() string         { return "daiki:queue:lease-principals" }
