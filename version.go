package dynago

import (
	"errors"
	"fmt"
	"strconv"
)

var (
	// ErrVersionMismatch means the item changed since the version the caller supplied: someone
	// else wrote it in between. Writes never retry it; the caller decides (reload, merge, report).
	ErrVersionMismatch = errors.New("dynago: item changed since the version supplied")
	// ErrVersionRequired means the write is declared `versioned: required` and no version was
	// supplied, or the entity passed with From was not read from the store.
	ErrVersionRequired = errors.New("dynago: this write requires the version that was read")
)

// FormatVersion renders a revision as an opaque version string, suitable for an ETag.
func FormatVersion(rev int64) string {
	if rev <= 0 {
		return ""
	}
	return strconv.FormatInt(rev, 36)
}

// ParseVersion parses a version string made by FormatVersion.
func ParseVersion(s string) (int64, error) {
	rev, err := strconv.ParseInt(s, 36, 64)
	if err != nil || rev <= 0 {
		return 0, fmt.Errorf("%w: malformed version %q", ErrVersionMismatch, s)
	}
	return rev, nil
}

// WriteOption modifies an update or delete.
type WriteOption func(*WriteOptions)

// WriteOptions are the resolved options of a write. Generated code reads them.
type WriteOptions struct {
	// Version is an expected version from IfVersion, or "" for none.
	Version string
	// From is an entity previously returned by the store, or nil.
	From any
	// NewVersion, if set, receives the item's version after a successful write.
	NewVersion *string
}

// IfVersion makes the write fail with ErrVersionMismatch unless the item is still at version v
// (from the entity's Version method, typically round-tripped through an ETag). An empty v means
// no check, so a missing If-Match header can be passed straight through.
//
// Writes that change nothing derived stay a single conditional UpdateItem; writes that maintain
// counters, claims or copies still read the item to compute their changes, but fail instead of
// retrying when the version differs.
func IfVersion(v string) WriteOption {
	return func(o *WriteOptions) { o.Version = v }
}

// From makes the write start from an entity the store returned earlier (by Get, a query, or a
// unique lookup) instead of reading the item again. The write fails with ErrVersionMismatch if
// the item has changed since that entity was read, so no update can be lost.
func From(entity any) WriteOption {
	return func(o *WriteOptions) { o.From = entity }
}

// ReturnVersion stores the item's new version in dst after a successful write, so a handler can
// return it as an ETag without reading the item again.
func ReturnVersion(dst *string) WriteOption {
	return func(o *WriteOptions) { o.NewVersion = dst }
}

// Written records a successful write's new revision for ReturnVersion.
func (o WriteOptions) Written(rev int64) {
	if o.NewVersion != nil {
		*o.NewVersion = FormatVersion(rev)
	}
}

// ApplyOptions resolves write options.
func ApplyOptions(opts []WriteOption) WriteOptions {
	var o WriteOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// ExpectedRev returns the revision the caller expects from IfVersion, 0 for none.
func (o WriteOptions) ExpectedRev() (int64, error) {
	if o.Version == "" {
		return 0, nil
	}
	return ParseVersion(o.Version)
}

// StaleAs reports a write that lost a race with another writer as ErrVersionMismatch when the
// caller supplied a version (the caller must decide what to do), and leaves it as ErrStale (which
// Retry retries) otherwise.
func StaleAs(versioned bool, err error) error {
	if versioned && errors.Is(err, ErrStale) {
		return ErrVersionMismatch
	}
	return err
}
