package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticgemini"
	"google.golang.org/genai"
)

const geminiCacheRegistryLimit = config.GeminiCacheRegistryEntries

// No credentials or raw prompt content are retained in the registry. The
// factory owns it across transient models; every lookup revalidates scope and
// the exact adapter projection with fresh request credentials.
type geminiCacheRegistry struct {
	mu      sync.Mutex
	entries map[string]*geminiCacheEntry
	now     func() time.Time
}
type geminiCacheCoverage struct {
	System, Tools bool
	Messages      int
}
type geminiCacheEntry struct {
	done     chan struct{}
	name     string
	expires  time.Time
	coverage geminiCacheCoverage
	err      error
}

func newGeminiCacheRegistry() *geminiCacheRegistry {
	return &geminiCacheRegistry{entries: make(map[string]*geminiCacheEntry), now: time.Now}
}
func validGeminiCacheName(name string) bool {
	id, ok := strings.CutPrefix(name, "cachedContents/")
	if !ok || id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func (r *geminiCacheRegistry) acquire(ctx context.Context, key string, coverage geminiCacheCoverage, create func(context.Context) (*genai.CachedContent, error)) (*geminiCacheEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	now := r.now()
	for k, e := range r.entries {
		if e.done == nil && !now.Before(e.expires) {
			delete(r.entries, k)
		}
	}
	if e := r.entries[key]; e != nil {
		done := e.done
		r.mu.Unlock()
		if done != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.err != nil {
			// The creator's cancellation or budget denial belongs to that call.
			// A live waiter uses its own complete request and budget, without
			// silently retrying resource creation or inheriting foreign errors.
			return nil, nil
		}
		if e.name == "" || !r.now().Before(e.expires) {
			return nil, nil
		}
		return e, nil
	}
	// When full, send the complete request instead of creating an untracked
	// billable resource or evicting a still-authorized in-flight operation.
	if len(r.entries) >= geminiCacheRegistryLimit {
		r.mu.Unlock()
		return nil, nil
	}
	e := &geminiCacheEntry{done: make(chan struct{}), coverage: coverage}
	done := e.done
	r.entries[key] = e
	r.mu.Unlock()
	resource, err := create(ctx)
	r.mu.Lock()
	e.err = err
	if err == nil && resource != nil && validGeminiCacheName(resource.Name) && r.now().Before(resource.ExpireTime) {
		e.name = resource.Name
		e.expires = resource.ExpireTime
	} else {
		delete(r.entries, key)
	}
	e.done = nil
	close(done)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if e.name == "" {
		return nil, nil
	}
	return e, nil
}
func (r *geminiCacheRegistry) invalidate(key string, e *geminiCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[key] == e {
		delete(r.entries, key)
	}
}

type geminiCachedModel struct {
	inner    *agenticgemini.Model
	registry *geminiCacheRegistry
	config   ResolvedModelConfig
}
type geminiCacheUse struct {
	key     string
	entry   *geminiCacheEntry
	suffix  []*schema.AgenticMessage
	options []model.Option
}

func (m *geminiCachedModel) prepare(ctx context.Context, in []*schema.AgenticMessage, opts []model.Option) (geminiCacheUse, error) {
	scope, err := sessionPromptCacheKey(ctx, m.config)
	if err != nil {
		return geminiCacheUse{}, err
	}
	// This first policy caches only system/tools. All conversation messages,
	// including function calls, results and signatures, remain an exact suffix.
	var prefix []*schema.AgenticMessage
	suffix := in
	if len(in) > 1 && in[0] != nil && in[0].Role == schema.AgenticRoleTypeSystem {
		prefix = in[:1]
		suffix = in[1:]
	}
	for _, msg := range suffix {
		if msg != nil && msg.Role == schema.AgenticRoleTypeSystem {
			return geminiCacheUse{}, nil
		}
	}
	modelName, projection, err := m.inner.PrefixCacheProjection(prefix, opts...)
	if err != nil {
		return geminiCacheUse{}, err
	}
	if len(suffix) == 0 || projection.SystemInstruction == nil && len(projection.Tools) == 0 {
		return geminiCacheUse{}, nil
	}
	data, err := json.Marshal(struct {
		Model  string
		Prefix *genai.CreateCachedContentConfig
	}{modelName, projection})
	if err != nil {
		return geminiCacheUse{}, invalid("Gemini cache projection cannot be encoded")
	}
	digest := sha256.Sum256(data)
	key := scope + ":" + hex.EncodeToString(digest[:])
	coverage := geminiCacheCoverage{System: projection.SystemInstruction != nil, Tools: len(projection.Tools) > 0, Messages: len(projection.Contents)}
	entry, err := m.registry.acquire(ctx, key, coverage, func(ctx context.Context) (*genai.CachedContent, error) {
		return m.inner.CreatePrefixCache(WithRequestPurpose(ctx, "cache_create"), prefix, opts...)
	})
	if err != nil {
		if ctx.Err() != nil {
			return geminiCacheUse{}, ctx.Err()
		}
		if vetoCode(err) != "" || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return geminiCacheUse{}, err
		}
		var api genai.APIError
		if errors.As(err, &api) && (api.Code == 401 || api.Code == 403) {
			return geminiCacheUse{}, err
		}
		// Resource creation is optional. No retry of that operation: a full,
		// equivalent generation still passes the same physical request observer.
		return geminiCacheUse{}, nil
	}
	if entry == nil {
		return geminiCacheUse{}, nil
	}
	cachedOpts := append([]model.Option(nil), opts...)
	cachedOpts = append(cachedOpts, agenticgemini.WithCachedContentName(entry.name))
	return geminiCacheUse{key: key, entry: entry, suffix: suffix, options: cachedOpts}, nil
}
func cacheResourceMissing(err error) bool {
	var api genai.APIError
	return errors.As(err, &api) && api.Code == 404
}
func (m *geminiCachedModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	use, err := m.prepare(ctx, in, opts)
	if err != nil {
		return nil, err
	}
	if use.entry == nil {
		return m.inner.Generate(ctx, in, opts...)
	}
	msg, err := m.inner.Generate(ctx, use.suffix, use.options...)
	if cacheResourceMissing(err) {
		m.registry.invalidate(use.key, use.entry)
		return m.inner.Generate(ctx, in, opts...)
	}
	return msg, err
}
func (m *geminiCachedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	use, err := m.prepare(ctx, in, opts)
	if err != nil {
		return nil, err
	}
	if use.entry == nil {
		return m.inner.Stream(ctx, in, opts...)
	}
	inner, err := m.inner.Stream(ctx, use.suffix, use.options...)
	if cacheResourceMissing(err) {
		m.registry.invalidate(use.key, use.entry)
		return m.inner.Stream(ctx, in, opts...)
	}
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer writer.Close()
		defer func() { inner.Close() }()
		accepted := false
		for {
			msg, recvErr := inner.Recv()
			if !accepted && cacheResourceMissing(recvErr) {
				m.registry.invalidate(use.key, use.entry)
				inner.Close()
				full, fullErr := m.inner.Stream(ctx, in, opts...)
				if fullErr != nil {
					writer.Send(nil, fullErr)
					return
				}
				inner = full
				// Exactly one complete fallback, never after exposing any stream chunk.
				accepted = true
				continue
			}
			if recvErr == io.EOF {
				return
			}
			if recvErr != nil {
				writer.Send(nil, recvErr)
				return
			}
			accepted = true
			if writer.Send(msg, nil) {
				return
			}
		}
	}()
	return reader, nil
}
