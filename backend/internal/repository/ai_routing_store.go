package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type aiRoutingStore struct {
	client *redis.Client
}

func NewAIRoutingStore(client *redis.Client) service.AIRoutingStore {
	if client == nil {
		return nil
	}
	return &aiRoutingStore{client: client}
}

const aiBindToolScript = `
local old = redis.call('GET', KEYS[1])
if old and old ~= ARGV[1] then
 redis.call('SET', KEYS[1], 'conflict', 'EX', 86400)
 return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'EX', 86400)
return 1`

func (s *aiRoutingStore) LoadToolBindings(ctx context.Context, keys []string) ([]string, error) {
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	rows := make([]string, len(values))
	for i, value := range values {
		rows[i], _ = value.(string)
	}
	return rows, nil
}

func (s *aiRoutingStore) SaveToolBinding(ctx context.Context, key, value string) error {
	stored, err := s.client.Eval(ctx, aiBindToolScript, []string{key}, value).Int()
	if err != nil {
		return fmt.Errorf("persist tool binding: %w", err)
	}
	if stored != 1 {
		return errors.New("tool binding conflicts with another execution")
	}
	return nil
}

func decisionTraceKey(id int64) string { return fmt.Sprintf("airoute:history:%d", id) }

func (s *aiRoutingStore) SaveTrace(ctx context.Context, id int64, trace service.AIRoutingTrace) error {
	raw, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	key := decisionTraceKey(id)
	_, err = s.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.LPush(ctx, key, raw)
		p.LTrim(ctx, key, 0, 49)
		p.Expire(ctx, key, 30*24*time.Hour)
		return nil
	})
	return err
}

func (s *aiRoutingStore) ListTraces(ctx context.Context, id int64) ([]service.AIRoutingTrace, error) {
	values, err := s.client.LRange(ctx, decisionTraceKey(id), 0, 49).Result()
	if err != nil {
		return nil, err
	}
	rows := make([]service.AIRoutingTrace, 0, len(values))
	for _, raw := range values {
		var trace service.AIRoutingTrace
		if err := json.Unmarshal([]byte(raw), &trace); err != nil {
			return nil, err
		}
		rows = append(rows, trace)
	}
	return rows, nil
}
