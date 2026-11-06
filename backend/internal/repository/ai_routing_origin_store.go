package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AIRoutingOriginStore = (*aiRoutingStore)(nil)
var _ service.AIRoutingOriginRevoker = (*aiRoutingStore)(nil)

const aiBindOriginScript = `
local old = redis.call('GET', KEYS[1])
local ttl = tonumber(ARGV[2])
if old == 'conflict' or (old and old ~= ARGV[1]) then
 ttl = math.max(ttl, redis.call('PTTL', KEYS[1]))
 redis.call('SET', KEYS[1], 'conflict', 'PX', ttl)
 return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
return 1`

func (s *aiRoutingStore) LoadOriginBindings(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return s.LoadToolBindings(ctx, keys)
}

func (s *aiRoutingStore) SaveOriginBinding(ctx context.Context, key, value string, ttl time.Duration) error {
	if key == "" || value == "" || ttl < time.Second || ttl > service.AIRoutingResponseOriginTTL {
		return errors.New("invalid AI response origin binding")
	}
	stored, err := s.client.Eval(ctx, aiBindOriginScript, []string{key}, value, ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("persist AI response origin: %w", err)
	}
	if stored != 1 {
		return errors.New("AI response origin conflicts with another execution")
	}
	return nil
}

func (s *aiRoutingStore) RevokeOriginBinding(ctx context.Context, key string, ttl time.Duration) error {
	if key == "" || ttl < time.Second || ttl > service.AIRoutingResponseOriginTTL {
		return errors.New("invalid AI response revocation")
	}
	return s.client.Set(ctx, key, "revoked", ttl).Err()
}
