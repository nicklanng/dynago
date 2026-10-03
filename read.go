package dynago

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// GetOne reads one item into out. It reports found=false rather than an error when the item
// does not exist.
func GetOne(ctx context.Context, t dynamo.Table, key Key, consistent bool, out any) (found bool, err error) {
	if err := key.Valid(); err != nil {
		return false, err
	}
	err = t.Get(AttrPK, key.PK).Range(AttrSK, dynamo.Equal, key.SK).Consistent(consistent).One(ctx, out)
	if errors.Is(err, dynamo.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// GetMany reads items by key with one BatchGetItem (chunked by the library at 100 keys) and
// appends the ones that exist to out, which must be a pointer to a slice.
func GetMany(ctx context.Context, t dynamo.Table, keys []Key, consistent bool, out any) error {
	if len(keys) == 0 {
		return nil
	}
	ks := make([]dynamo.Keyed, len(keys))
	for i, k := range keys {
		if err := k.Valid(); err != nil {
			return err
		}
		ks[i] = dynamo.Keys{k.PK, k.SK}
	}
	err := t.Batch(AttrPK, AttrSK).Get(ks...).Consistent(consistent).All(ctx, out)
	if errors.Is(err, dynamo.ErrNotFound) {
		return nil
	}
	return err
}

// GetBatch reads several items by key and returns those that exist, in the order of keys (a key
// given twice is read once, and returned at its first place). It makes one BatchGetItem per 100
// keys. Each item is charged as a GetItem of it would be.
func GetBatch(ctx context.Context, t dynamo.Table, keys []Key, consistent bool) ([]dynamo.Item, error) {
	place := make(map[Key]int, len(keys))
	distinct := make([]Key, 0, len(keys))
	for _, k := range keys {
		if _, seen := place[k]; !seen {
			place[k] = len(distinct)
			distinct = append(distinct, k)
		}
	}
	var raws []dynamo.Item
	if err := GetMany(ctx, t, distinct, consistent, &raws); err != nil {
		return nil, err
	}
	found := make([]dynamo.Item, len(distinct))
	for _, raw := range raws {
		if i, ok := place[ItemKey(raw)]; ok {
			found[i] = raw
		}
	}
	out := raws[:0]
	for _, raw := range found {
		if raw != nil {
			out = append(out, raw)
		}
	}
	return out, nil
}

// SplitKey reads the placeholder values back out of a rendered key. literals are the template's
// literal parts in order, one more than it has placeholders: the text before the first
// placeholder, between each two, and after the last ("" where there is none; only the first and
// last may be empty). It reports false if the key doesn't have that shape.
//
// Reading back is exact because a value may not contain the first character of the literal that
// follows it (CheckKeyPart), so the first occurrence of that literal ends the value.
func SplitKey(key string, literals ...string) ([]string, bool) {
	if len(literals) < 2 || !strings.HasPrefix(key, literals[0]) {
		return nil, false
	}
	rest := key[len(literals[0]):]
	n := len(literals) - 1
	out := make([]string, 0, n)
	for i := 1; i < n; i++ {
		at := strings.Index(rest, literals[i])
		if literals[i] == "" || at < 0 {
			return nil, false
		}
		out = append(out, rest[:at])
		rest = rest[at+len(literals[i]):]
	}
	if !strings.HasSuffix(rest, literals[n]) {
		return nil, false
	}
	return append(out, rest[:len(rest)-len(literals[n])]), true
}

// QuerySpec describes one generated query.
type QuerySpec struct {
	// Scope identifies the access pattern and partition; a cursor is only valid for the scope
	// that produced it.
	Scope string
	// Index is the GSI name, or "" for the base table.
	Index  string
	PKAttr string
	PK     string
	SKAttr string
	// Prefix restricts the query to sort keys starting with it (the item family's prefix).
	Prefix string
	// From and To, when set, bound the sort key inclusively. They already include Prefix.
	From, To   *string
	Desc       bool
	Consistent bool
	PageSize   int
	MaxPage    int
	// TTLAttr, if set, leaves out items whose TTL has passed but that DynamoDB has not yet deleted.
	// Such a page may hold fewer items than its size and still have a next cursor.
	TTLAttr string
	// Types, if set, keeps only items of these types (_t): a read of a whole partition returns
	// some of the kinds it holds. A page evaluates up to its size of the partition's items, of
	// every kind, so it may hold fewer and still have a next cursor.
	Types []string
	// Project, if set, reads only these attributes. It saves bandwidth and keeps other attributes
	// from callers; DynamoDB still charges for the whole item.
	Project []string
}

// Query runs exactly one Query request for one page and appends the items to out, which must be
// a pointer to a slice. It returns the cursor for the next page, or "" when there are no more.
func Query(ctx context.Context, t dynamo.Table, spec QuerySpec, page Page, out any) (string, error) {
	if err := (Key{PK: spec.PK}).Valid(); err != nil {
		return "", err
	}
	q := t.Get(spec.PKAttr, spec.PK)
	if spec.Index != "" {
		q.Index(spec.Index)
	}
	switch {
	case spec.From != nil && spec.To != nil:
		q.Range(spec.SKAttr, dynamo.Between, *spec.From, *spec.To+RangeEnd)
	case spec.From != nil && spec.Prefix != "":
		q.Range(spec.SKAttr, dynamo.Between, *spec.From, spec.Prefix+RangeEnd)
	case spec.From != nil:
		q.Range(spec.SKAttr, dynamo.GreaterOrEqual, *spec.From)
	case spec.To != nil && spec.Prefix != "":
		q.Range(spec.SKAttr, dynamo.Between, spec.Prefix, *spec.To+RangeEnd)
	case spec.To != nil:
		// Key attributes cannot be compared with "", so an open lower end without a prefix is <=.
		q.Range(spec.SKAttr, dynamo.LessOrEqual, *spec.To+RangeEnd)
	case spec.Prefix != "" && spec.SKAttr != "":
		q.Range(spec.SKAttr, dynamo.BeginsWith, spec.Prefix)
	}
	if spec.TTLAttr != "" {
		q.Filter("attribute_not_exists($) OR $ > ?", spec.TTLAttr, spec.TTLAttr, Now())
	}
	if len(spec.Types) > 0 {
		args := []any{AttrType}
		for _, t := range spec.Types {
			args = append(args, t)
		}
		q.Filter("$ IN (?"+strings.Repeat(", ?", len(spec.Types)-1)+")", args...)
	}
	if len(spec.Project) > 0 {
		paths := make([]string, len(spec.Project))
		for i, a := range spec.Project {
			paths[i] = Path(a)
		}
		q.Project(paths...)
	}
	size := page.Size
	if size <= 0 {
		size = spec.PageSize
	}
	if size > spec.MaxPage {
		size = spec.MaxPage
	}
	q.SearchLimit(size)
	if spec.Desc {
		q.Order(dynamo.Descending)
	}
	if spec.Consistent {
		q.Consistent(true)
	}
	scope := cursorScope(spec)
	if page.Cursor != "" {
		lek, err := decodeCursor(scope, page.Cursor)
		if err != nil {
			return "", err
		}
		q.StartFrom(lek)
	}
	lek, err := q.AllWithLastEvaluatedKey(ctx, out)
	if err != nil {
		return "", err
	}
	return encodeCursor(scope, lek)
}

// ScanSpec describes one generated scan: a declared read of every item of one entity.
type ScanSpec struct {
	// Scope identifies the access pattern; a cursor is only valid for the scope that produced it.
	Scope string
	// Type is the entity's type attribute (_t): the scan returns only its items.
	Type       string
	Consistent bool
	PageSize   int
	MaxPage    int
	// TTLAttr, if set, leaves out items whose TTL has passed.
	TTLAttr string
}

// Scan runs exactly one Scan request for one page and appends the entity's items to out, which
// must be a pointer to a slice. A page evaluates up to its size of the table's items, of every
// kind, and keeps the entity's: it can hold few items, or none, and still have a next cursor. It
// returns the cursor for the next page, or "" when the whole table has been read.
func Scan(ctx context.Context, t dynamo.Table, spec ScanSpec, page Page, out any) (string, error) {
	s := t.Scan().Filter("$ = ?", AttrType, spec.Type)
	if spec.TTLAttr != "" {
		s.Filter("attribute_not_exists($) OR $ > ?", spec.TTLAttr, spec.TTLAttr, Now())
	}
	size := page.Size
	if size <= 0 {
		size = spec.PageSize
	}
	if size > spec.MaxPage {
		size = spec.MaxPage
	}
	s.SearchLimit(size)
	if spec.Consistent {
		s.Consistent(true)
	}
	scope := spec.Scope + "|scan"
	if page.Cursor != "" {
		lek, err := decodeCursor(scope, page.Cursor)
		if err != nil {
			return "", err
		}
		s.StartFrom(lek)
	}
	lek, err := s.AllWithLastEvaluatedKey(ctx, out)
	if err != nil {
		return "", err
	}
	return encodeCursor(scope, lek)
}

// cursorScope ties a cursor to everything that decides which items a page can hold: the access
// pattern, the shape of its keys and its partition (spec.Scope) and the range bounds. A cursor from other
// bounds could start outside them, which DynamoDB rejects.
func cursorScope(spec QuerySpec) string {
	bound := func(b *string) string {
		if b == nil {
			return "-"
		}
		return "=" + *b
	}
	return spec.Scope + "|" + bound(spec.From) + "|" + bound(spec.To)
}

type cursor struct {
	Scope string            `json:"s"`
	Key   map[string]string `json:"k"`
}

// cursorKeys holds the signing key first, then keys still accepted for verification.
var cursorKeys atomic.Pointer[[][]byte]

// SignCursors makes page cursors carry an HMAC-SHA256 under key, so a client cannot forge or edit
// one. Every instance must use the same key. Cursors signed with any of previous are still
// accepted, so the key can be rotated: deploy the new key with the old one as previous, then drop
// the old one once its cursors have expired from clients. SignCursors(nil) turns signing off.
// It is safe to call while queries run.
//
// Without signing, cursors are only checked to belong to the query, partition and range bounds
// that produced them.
func SignCursors(key []byte, previous ...[]byte) {
	if len(key) == 0 {
		cursorKeys.Store(nil)
		return
	}
	keys := [][]byte{append([]byte(nil), key...)}
	for _, p := range previous {
		keys = append(keys, append([]byte(nil), p...))
	}
	cursorKeys.Store(&keys)
}

func signingKeys() [][]byte {
	if k := cursorKeys.Load(); k != nil {
		return *k
	}
	return nil
}

func mac(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return h.Sum(nil)
}

// encodeCursor makes an opaque cursor. Every key attribute dynago writes is a string, so the
// last evaluated key is stored as plain strings.
func encodeCursor(scope string, lek dynamo.PagingKey) (string, error) {
	if len(lek) == 0 {
		return "", nil
	}
	c := cursor{Scope: scope, Key: make(map[string]string, len(lek))}
	for k, v := range lek {
		s, ok := v.(*types.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("dynago: key attribute %s is not a string", k)
		}
		c.Key[k] = s.Value
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	out := base64.RawURLEncoding.EncodeToString(b)
	if keys := signingKeys(); len(keys) > 0 {
		out += "." + base64.RawURLEncoding.EncodeToString(mac(keys[0], b))
	}
	return out, nil
}

func decodeCursor(scope, s string) (dynamo.PagingKey, error) {
	payload, sig, signed := strings.Cut(s, ".")
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	if keys := signingKeys(); len(keys) > 0 {
		got, err := base64.RawURLEncoding.DecodeString(sig)
		valid := false
		for _, k := range keys {
			valid = valid || (signed && err == nil && hmac.Equal(got, mac(k, b)))
		}
		if !valid {
			return nil, fmt.Errorf("%w: bad signature", ErrInvalidCursor)
		}
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || len(c.Key) == 0 {
		return nil, ErrInvalidCursor
	}
	if c.Scope != scope {
		return nil, fmt.Errorf("%w: it belongs to a different query, partition or range, or to how the query was keyed before", ErrInvalidCursor)
	}
	lek := make(dynamo.PagingKey, len(c.Key))
	for k, v := range c.Key {
		lek[k] = &types.AttributeValueMemberS{Value: v}
	}
	return lek, nil
}
