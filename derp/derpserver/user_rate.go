// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	xrate "golang.org/x/time/rate"
	"tailscale.com/derp"
	"tailscale.com/tailcfg"
)

const maxUserRateConfigSize = 1 << 20

type userRatePolicy struct {
	uploadBytesPerSecond   uint64
	downloadBytesPerSecond uint64
	burstBytes             int
}

type userRateConfig struct {
	defaultPolicy userRatePolicy
	userPolicies  map[tailcfg.UserID]userRatePolicy
	taggedPolicy  userRatePolicy
}

type rawUserRatePolicy struct {
	UploadBytesPerSecond   *uint64 `json:"upload_bytes_per_second"`
	DownloadBytesPerSecond *uint64 `json:"download_bytes_per_second"`
	BurstBytes             *uint64 `json:"burst_bytes"`
}

type rawUserRateConfig struct {
	Default *rawUserRatePolicy           `json:"default"`
	Users   map[string]rawUserRatePolicy `json:"users"`
	Tagged  *rawUserRatePolicy           `json:"tagged"`
}

// LoadUserRateConfig reads and validates a user-rate configuration file.
func LoadUserRateConfig(path string) (userRateConfig, error) {
	if path == "" {
		return userRateConfig{}, errors.New("user rate config path is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return userRateConfig{}, fmt.Errorf("reading user rate config: %w", err)
	}
	defer file.Close()

	contents, err := io.ReadAll(io.LimitReader(file, maxUserRateConfigSize+1))
	if err != nil {
		return userRateConfig{}, fmt.Errorf("reading user rate config: %w", err)
	}
	if len(contents) > maxUserRateConfigSize {
		return userRateConfig{}, fmt.Errorf("user rate config exceeds %d bytes", maxUserRateConfigSize)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var raw rawUserRateConfig
	if err := decoder.Decode(&raw); err != nil {
		return userRateConfig{}, fmt.Errorf("parsing user rate config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return userRateConfig{}, errors.New("user rate config has trailing JSON values")
		}
		return userRateConfig{}, fmt.Errorf("parsing user rate config: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		return userRateConfig{}, fmt.Errorf("parsing user rate config: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(fields["users"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(fields["tagged"]), []byte("null")) {
		return userRateConfig{}, errors.New("user rate config does not permit null optional policies")
	}
	if raw.Default == nil {
		return userRateConfig{}, errors.New("user rate config requires a default policy")
	}
	defaultPolicy, err := raw.Default.normalize("default")
	if err != nil {
		return userRateConfig{}, err
	}
	config := userRateConfig{
		defaultPolicy: defaultPolicy,
		userPolicies:  make(map[tailcfg.UserID]userRatePolicy, len(raw.Users)),
		taggedPolicy:  defaultPolicy,
	}
	for rawID, rawPolicy := range raw.Users {
		userID, err := parseUserRateID(rawID)
		if err != nil {
			return userRateConfig{}, err
		}
		policy, err := rawPolicy.normalize("users." + rawID)
		if err != nil {
			return userRateConfig{}, err
		}
		config.userPolicies[userID] = policy
	}
	if raw.Tagged != nil {
		policy, err := raw.Tagged.normalize("tagged")
		if err != nil {
			return userRateConfig{}, err
		}
		config.taggedPolicy = policy
	}
	return config, nil
}

func (raw rawUserRatePolicy) normalize(name string) (userRatePolicy, error) {
	if raw.UploadBytesPerSecond == nil || raw.DownloadBytesPerSecond == nil {
		return userRatePolicy{}, fmt.Errorf("user rate policy %q requires both directional rates", name)
	}
	burst := uint64(derp.MaxPacketSize)
	if raw.BurstBytes != nil {
		burst = *raw.BurstBytes
	}
	if burst != 0 && burst < derp.MaxPacketSize {
		return userRatePolicy{}, fmt.Errorf("user rate policy %q burst is below the maximum packet size", name)
	}
	if burst == 0 {
		burst = derp.MaxPacketSize
	}
	maxInt := uint64(^uint(0) >> 1)
	if burst > maxInt {
		return userRatePolicy{}, fmt.Errorf("user rate policy %q burst does not fit in int", name)
	}
	return userRatePolicy{
		uploadBytesPerSecond:   *raw.UploadBytesPerSecond,
		downloadBytesPerSecond: *raw.DownloadBytesPerSecond,
		burstBytes:             int(burst),
	}, nil
}

func parseUserRateID(raw string) (tailcfg.UserID, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	maxUserID := uint64(^uint64(0) >> 1)
	if err != nil || id == 0 || id > maxUserID || strconv.FormatUint(id, 10) != raw {
		return 0, fmt.Errorf("invalid user rate policy ID %q", raw)
	}
	return tailcfg.UserID(id), nil
}

type userRateSubject struct {
	userID tailcfg.UserID
	tagged bool
}

type userRateBuckets struct {
	upload   *xrate.Limiter
	download *xrate.Limiter
}

func newUserRateBuckets(policy userRatePolicy, now time.Time) *userRateBuckets {
	buckets := &userRateBuckets{}
	buckets.update(policy, now)
	return buckets
}

func (buckets *userRateBuckets) update(policy userRatePolicy, now time.Time) {
	buckets.upload = updateUserRateLimiter(buckets.upload, policy.uploadBytesPerSecond, policy.burstBytes, now)
	buckets.download = updateUserRateLimiter(buckets.download, policy.downloadBytesPerSecond, policy.burstBytes, now)
}

func updateUserRateLimiter(limiter *xrate.Limiter, bytesPerSecond uint64, burst int, now time.Time) *xrate.Limiter {
	if bytesPerSecond == 0 {
		return nil
	}
	limit := xrate.Limit(bytesPerSecond)
	if limiter == nil {
		return xrate.NewLimiter(limit, burst)
	}
	limiter.SetLimitAt(now, limit)
	limiter.SetBurstAt(now, burst)
	return limiter
}

type userRateRegistry struct {
	mu      sync.RWMutex
	config  userRateConfig
	buckets map[userRateSubject]*userRateBuckets
}

func newUserRateRegistry(config userRateConfig) *userRateRegistry {
	return &userRateRegistry{
		config:  config,
		buckets: make(map[userRateSubject]*userRateBuckets),
	}
}

func (registry *userRateRegistry) apply(config userRateConfig) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	registry.config = config
	now := time.Now()
	for subject, buckets := range registry.buckets {
		buckets.update(registry.policyFor(subject), now)
	}
}

func (registry *userRateRegistry) allowUpload(subject userRateSubject, size int) bool {
	return registry.allow(subject, size, true)
}

func (registry *userRateRegistry) allowDownload(subject userRateSubject, size int) bool {
	return registry.allow(subject, size, false)
}

func (registry *userRateRegistry) allow(subject userRateSubject, size int, upload bool) bool {
	registry.mu.RLock()
	buckets := registry.buckets[subject]
	if buckets != nil {
		allowed := buckets.allow(size, upload)
		registry.mu.RUnlock()
		return allowed
	}
	registry.mu.RUnlock()

	registry.mu.Lock()
	defer registry.mu.Unlock()
	buckets = registry.buckets[subject]
	if buckets == nil {
		buckets = newUserRateBuckets(registry.policyFor(subject), time.Now())
		registry.buckets[subject] = buckets
	}
	return buckets.allow(size, upload)
}

func (registry *userRateRegistry) policyFor(subject userRateSubject) userRatePolicy {
	if subject.tagged {
		return registry.config.taggedPolicy
	}
	if policy, ok := registry.config.userPolicies[subject.userID]; ok {
		return policy
	}
	return registry.config.defaultPolicy
}

func (buckets *userRateBuckets) allow(size int, upload bool) bool {
	limiter := buckets.download
	if upload {
		limiter = buckets.upload
	}
	return limiter == nil || limiter.AllowN(time.Now(), size)
}
