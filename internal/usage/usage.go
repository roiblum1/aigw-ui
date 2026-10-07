// Package usage reads the token counters that the gateways' rate limit
// services keep in the shared Redis. The only change it can make is to delete
// a counter, and only when resets are allowed.
package usage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// QuotaKeyPattern matches every key of the gateway's quota domain.
const QuotaKeyPattern = "*ai-gateway-quota_*"

type Options struct {
	// URL is redis://[user:password@]host:port[/db], or rediss:// for TLS.
	// Redis behind an OpenShift Route needs rediss:// and port 443.
	URL string
	// CAFile is a PEM file with the CA that signed the Redis certificate.
	CAFile string
	// TLSInsecure skips verification of the Redis certificate.
	TLSInsecure bool
	// KeyPrefix is the rate limit service's CACHE_KEY_PREFIX, usually empty.
	KeyPrefix string
	// AllowReset lets Delete remove counters. Off, Redis is only read.
	AllowReset bool
}

type Reader struct {
	rdb        *redis.Client
	Prefix     string
	AllowReset bool
}

func New(o Options) (*Reader, error) {
	opts, err := redis.ParseURL(o.URL)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL: %w", err)
	}
	if opts.TLSConfig != nil {
		opts.TLSConfig.InsecureSkipVerify = o.TLSInsecure
		opts.TLSConfig.MinVersion = tls.VersionTLS12
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("REDIS_CA_FILE: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("REDIS_CA_FILE holds no certificate")
			}
			opts.TLSConfig.RootCAs = pool
		}
	} else if o.CAFile != "" || o.TLSInsecure {
		return nil, errors.New("REDIS_CA_FILE and REDIS_TLS_INSECURE need a rediss:// URL")
	}
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	opts.PoolSize = 4
	// Monitoring must never get in the way of the rate limit service.
	opts.ClientName = "aigw-ui"
	return &Reader{rdb: redis.NewClient(opts), Prefix: o.KeyPrefix, AllowReset: o.AllowReset}, nil
}

func (r *Reader) Close() error { return r.rdb.Close() }

func (r *Reader) Ping(ctx context.Context) error { return r.rdb.Ping(ctx).Err() }

// Values returns the counter stored under each key. A key that does not exist
// is left out: nothing was used in that window yet.
func (r *Reader) Values(ctx context.Context, keys []string) (map[string]int64, error) {
	out := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out[keys[i]] = n
		}
	}
	return out, nil
}

// Delete removes counters, which sets their usage back to zero until the
// window ends. It returns how many existed.
func (r *Reader) Delete(ctx context.Context, keys []string) (int64, error) {
	if !r.AllowReset {
		return 0, errors.New("resetting usage is turned off")
	}
	if len(keys) == 0 {
		return 0, nil
	}
	return r.rdb.Del(ctx, keys...).Result()
}

// Sample looks for keys of the quota domain without knowing their exact
// names. It reports how many it saw in a bounded scan and one of them, to
// tell "nothing was used yet" apart from "the keys are named differently".
func (r *Reader) Sample(ctx context.Context) (int, string, error) {
	var cursor uint64
	seen, sample := 0, ""
	for range 20 {
		keys, next, err := r.rdb.Scan(ctx, cursor, QuotaKeyPattern, 500).Result()
		if err != nil {
			return seen, sample, err
		}
		seen += len(keys)
		if sample == "" && len(keys) > 0 {
			sample = keys[0]
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return seen, sample, nil
}
