package dynago

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// Timestamps say when an item was first written and last changed. They come from the clock of the
// server that wrote it, so across servers they are ordered only as well as those clocks agree.
// A zero time means unknown: the item was written before dynago kept timestamps.
type Timestamps struct {
	Created, Updated time.Time
}

// NewStamp returns the current time as dynago stores it in _created and _updated.
func NewStamp() string { return FmtTime(time.Now()) }

// FmtStamp renders a timestamp for storage; the zero time is "".
func FmtStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return FmtTime(t)
}

// ParseStamp reads a stored timestamp; "" (or anything unreadable) is the zero time.
func ParseStamp(s string) time.Time {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// StampsOf reads the timestamps of a stored item.
func StampsOf(raw dynamo.Item) Timestamps {
	get := func(attr string) time.Time {
		if s, ok := raw[attr].(*types.AttributeValueMemberS); ok {
			return ParseStamp(s.Value)
		}
		return time.Time{}
	}
	return Timestamps{Created: get(AttrCreated), Updated: get(AttrUpdated)}
}
