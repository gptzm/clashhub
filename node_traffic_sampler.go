package main

import (
	"encoding/json"
	"errors"
	"time"
)

// mihomoConnectionSample mirrors the live /connections response. These counters
// cover active trackers only; closed trackers never produce a final record.
type mihomoConnectionSample struct {
	ID       string    `json:"id"`
	Start    time.Time `json:"start"`
	Chains   []string  `json:"chains"`
	Upload   uint64    `json:"upload"`
	Download uint64    `json:"download"`
}

func (c *mihomoConnectionSample) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID       string    `json:"id"`
		Start    time.Time `json:"start"`
		Chains   []string  `json:"chains"`
		Upload   *uint64   `json:"upload"`
		Download *uint64   `json:"download"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.ID == "" || len(raw.ID) > 128 || raw.Start.IsZero() || raw.Upload == nil || raw.Download == nil || len(raw.Chains) > 64 {
		return errors.New("invalid connection sample")
	}
	*c = mihomoConnectionSample{ID: raw.ID, Start: raw.Start, Chains: raw.Chains, Upload: *raw.Upload, Download: *raw.Download}
	return nil
}

type mihomoConnectionsSnapshot struct {
	UploadTotal   uint64                   `json:"uploadTotal"`
	DownloadTotal uint64                   `json:"downloadTotal"`
	Connections   []mihomoConnectionSample `json:"connections"`
}

func (s *mihomoConnectionsSnapshot) UnmarshalJSON(data []byte) error {
	var raw struct {
		UploadTotal   *uint64         `json:"uploadTotal"`
		DownloadTotal *uint64         `json:"downloadTotal"`
		Connections   json.RawMessage `json:"connections"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.UploadTotal == nil || raw.DownloadTotal == nil || raw.Connections == nil {
		return errors.New("missing snapshot fields")
	}
	var connections []mihomoConnectionSample
	if err := json.Unmarshal(raw.Connections, &connections); err != nil {
		return err
	}
	s.UploadTotal, s.DownloadTotal, s.Connections = *raw.UploadTotal, *raw.DownloadTotal, connections
	return nil
}

type nodeTrafficBytes struct{ Upload, Download uint64 }
type nodeTrafficObservation struct {
	// Known and Unknown are only bytes visible in connection snapshots. They do
	// not include the unobserved tails of closed or entirely short connections.
	Known   map[string]nodeTrafficBytes
	Unknown nodeTrafficBytes
	// Global is the controller's aggregate difference, including closed flows.
	// It is not atomic with the tracker counters and must not be subtracted from
	// Known to produce an exact unknown value for this interval.
	Global        nodeTrafficBytes
	Elapsed       time.Duration
	RateAvailable bool
}
type nodeTrafficCursor struct {
	start            time.Time
	node             string
	upload, download uint64
}
type nodeTrafficSampler struct {
	epoch  string
	at     time.Time
	total  nodeTrafficBytes
	active map[string]nodeTrafficCursor
}

// observe requires a stable engine-process epoch and a conservative resolver.
// The resolver must only return a concrete outbound if the chain proves it;
// ambiguous, direct, reject and unrecognized chains may return empty. Calls
// must be serialized. A first sample or new epoch establishes a baseline and
// intentionally does not claim traffic from before observation began.
func (s *nodeTrafficSampler) observe(epoch string, at time.Time, snap mihomoConnectionsSnapshot, resolve func([]string) string, maxRateGap time.Duration) (nodeTrafficObservation, error) {
	var out nodeTrafficObservation
	if epoch == "" || at.IsZero() || resolve == nil || maxRateGap <= 0 {
		return out, errors.New("invalid traffic sampling parameters")
	}
	if s.epoch == epoch && !s.at.IsZero() && !at.After(s.at) {
		return out, errors.New("non-increasing traffic sample time")
	}
	next := make(map[string]nodeTrafficCursor, len(snap.Connections))
	for _, c := range snap.Connections {
		if c.ID == "" || c.Start.IsZero() {
			return out, errors.New("connection missing identity")
		}
		if _, exists := next[c.ID]; exists {
			return out, errors.New("duplicate connection identity")
		}
		next[c.ID] = nodeTrafficCursor{start: c.Start, node: resolve(c.Chains), upload: c.Upload, download: c.Download}
	}
	total := nodeTrafficBytes{snap.UploadTotal, snap.DownloadTotal}
	baseline := s.epoch != epoch || s.at.IsZero() || total.Upload < s.total.Upload || total.Download < s.total.Download
	out.Known = make(map[string]nodeTrafficBytes)
	if !baseline {
		out.Elapsed = at.Sub(s.at)
		out.RateAvailable = out.Elapsed <= maxRateGap
		out.Global = nodeTrafficBytes{total.Upload - s.total.Upload, total.Download - s.total.Download}
		for id, c := range next {
			old, exists := s.active[id]
			var delta nodeTrafficBytes
			node := c.node
			if exists {
				// UUID reuse or a counter regression makes continuity unknowable.
				if !old.start.Equal(c.start) || c.upload < old.upload || c.download < old.download {
					continue
				}
				delta = nodeTrafficBytes{c.upload - old.upload, c.download - old.download}
				if old.node != c.node {
					node = ""
				}
			} else {
				// A connection first visible after the previous snapshot can already have
				// bytes, but its creation must be after that snapshot to avoid recounting.
				if !c.start.After(s.at) {
					continue
				}
				delta = nodeTrafficBytes{c.upload, c.download}
			}
			if node == "" {
				out.Unknown.Upload = statsSaturatingAdd(out.Unknown.Upload, delta.Upload)
				out.Unknown.Download = statsSaturatingAdd(out.Unknown.Download, delta.Download)
			} else {
				value := out.Known[node]
				value.Upload = statsSaturatingAdd(value.Upload, delta.Upload)
				value.Download = statsSaturatingAdd(value.Download, delta.Download)
				out.Known[node] = value
			}
		}
	}
	s.epoch, s.at, s.total, s.active = epoch, at, total, next
	return out, nil
}
