// Copyright 2017-2020, Square, Inc.

// Package etre provides API clients and low-level primitive data types.
package etre

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"runtime"
	"sort"
)

const (
	VERSION                  = "0.12.0"
	API_ROOT          string = "/api/v1"
	META_LABEL_ID            = "_id"
	META_LABEL_TYPE          = "_type"
	META_LABEL_REV           = "_rev"
	CDC_WRITE_TIMEOUT int    = 5 // seconds

	VERSION_HEADER       = "X-Etre-Version"
	TRACE_HEADER         = "X-Etre-Trace"
	QUERY_TIMEOUT_HEADER = "X-Etre-Query-Timeout"
)

var (
	ErrTypeMismatch   = errors.New("entity _type and Client entity type are different")
	ErrIdSet          = errors.New("entity _id is set but not allowed on insert")
	ErrIdNotSet       = errors.New("entity _id is not set")
	ErrNoEntity       = errors.New("empty entity or id slice; at least one required")
	ErrNoLabel        = errors.New("empty label slice; at least one required")
	ErrNoQuery        = errors.New("empty query string")
	ErrBadData        = errors.New("data from CDC feed is not event or control")
	ErrCallerBlocked  = errors.New("caller blocked")
	ErrEntityNotFound = errors.New("entity not found")
	ErrClientTimeout  = errors.New("client timeout")
)

// Entity represents a single Etre entity. The caller is responsible for knowing
// or determining the type of value for each key.
//
// If label _type is set, the Client verifies that it matches its type. For example,
// if _type = "foo", Insert or Update with a Client bound to entity type "bar"
// returns ErrTypeMismatch. If label _type is not set, the Client entity type is
// presumed.
//
// Label _id cannot be set on insert. If set, Insert returns ErrIdSet. On update,
// label _id must be set; if not, Update returns ErrIdNotSet. _id corresponds to
// WriteResult.Writes[].Id.
type Entity map[string]interface{}

func (e Entity) Id() string {
	return e[META_LABEL_ID].(string)
}

func (e Entity) Type() string {
	return e[META_LABEL_TYPE].(string)
}

func (e Entity) Rev() int64 {
	// See "Some other useful marshalling mappings are:" at https://pkg.go.dev/go.mongodb.org/mongo-driver/bson?tab=doc
	// TL;DR: only int32 and int64 map 1:1 Go:BSON. Before v0.11, we used int
	// but that is magical in BSON: "int marshals to a BSON int32 if the value
	// is between math.MinInt32 and math.MaxInt32, inclusive, and a BSON int64
	// otherwise." As of v0.11 _rev is int64 everywhere, but for backwards-compat
	// we check for int and int32.
	v := e[META_LABEL_REV]
	switch v.(type) {
	case int64:
		return v.(int64)
	case int32:
		return int64(v.(int32))
	case int:
		return int64(v.(int))
	}
	panic(fmt.Sprintf("entity %s has invalid _rev data type: %T; expected int64 (or int/int32 before v0.11)",
		e.Id(), v))
}

// Has returns true of the entity has the label, regardless of its value.
func (e Entity) Has(label string) bool {
	_, ok := e[label]
	return ok
}

// A Set is a user-defined logical grouping of writes (insert, update, delete).
type Set struct {
	Id   string
	Op   string
	Size int
}

func (e Entity) Set() Set {
	set := Set{}
	if _, ok := e["_setId"]; ok {
		set.Id = e["_setId"].(string)
	}
	if _, ok := e["_setOp"]; ok {
		set.Op = e["_setOp"].(string)
	}
	if _, ok := e["_setSize"]; ok {
		var size int
		switch e["_setSize"].(type) {
		case int64:
			size = int(e["_setSize"].(int64))
		case int32:
			size = int(e["_setSize"].(int32))
		case int:
			size = e["_setSize"].(int)
		}
		set.Size = size
	}
	return set
}

var metaLabels = map[string]bool{
	"_id":      true,
	"_rev":     true,
	"_setId":   true,
	"_setOp":   true,
	"_setSize": true,
	"_ts":      true,
	"_type":    true,
}

func IsMetalabel(label string) bool {
	return metaLabels[label]
}

// Labels returns all labels, sorted, including meta-labels (_id, _type, etc.)
func (e Entity) Labels() []string {
	labels := make([]string, len(e))
	i := 0
	for label := range e {
		labels[i] = label
		i++
	}
	sort.Strings(labels)
	return labels
}

// String returns the string value of the label. If the label is not set or
// its value is not a string, an empty string is returned.
func (e Entity) String(label string) string {
	v := e[label]
	switch v.(type) {
	case string:
		return v.(string)
	}
	return ""
}

// QueryFilter represents filtering options for EntityClient.Query().
type QueryFilter struct {
	// ReturnLabels defines labels included in matching entities. An empty slice
	// returns all labels, including meta-labels. Else, only labels in the slice
	// are returned.
	ReturnLabels []string

	// Distinct returns unique entities if ReturnLabels contains a single value.
	// Etre returns an error if enabled and ReturnLabels has more than one value.
	Distinct bool

	// UseRWStore is a hint to the server to service the query from the writer
	// store.  This is useful to force read-after-write consistency.
	UseRWStore bool
}

// WriteResult represents the result of a write operation (insert, update delete).
// On success or failure, all write ops return a WriteResult.
//
// If Error is set (not nil), some or all writes failed. Writes stop on the first
// error, so len(Writes) = index into slice of entities sent by client that failed.
// For example, if the first entity causes an error, len(Writes) = 0. If the third
// entity fails, len(Writes) = 2 (zero indexed).
type WriteResult struct {
	Writes []Write `json:"writes"`          // successful writes
	Error  *Error  `json:"error,omitempty"` // error before, during, or after writes
}

func (wr WriteResult) IsZero() bool {
	return wr.Error == nil && len(wr.Writes) == 0
}

// Write represents the successful write of one entity.
type Write struct {
	EntityId string `json:"entityId"`       // internal _id of entity (all write ops)
	URI      string `json:"uri,omitempty"`  // fully-qualified address of new entity (insert)
	Diff     Entity `json:"diff,omitempty"` // previous entity label values (update)
}

// Error is the standard response for all handled errors. Client errors (HTTP 400
// codes) and internal errors (HTTP 500 codes) are returned as an Error, if handled.
// If not handled (API crash, panic, etc.), Etre returns an HTTP 500 code and the
// response data is undefined; the client should print any response data as a string.
type Error struct {
	Message    string `json:"message"`    // human-readable and loggable error message
	Type       string `json:"type"`       // error slug (e.g. db-error, missing-param, etc.)
	EntityId   string `json:"entityId"`   // entity ID that caused error, if any
	HTTPStatus int    `json:"httpStatus"` // HTTP status code
}

func (e Error) New(msgFmt string, msgArgs ...interface{}) Error {
	if msgFmt != "" {
		e.Message = fmt.Sprintf(msgFmt, msgArgs...)
	}
	return e
}

func (e Error) String() string {
	return fmt.Sprintf("Etre error %s: %s", e.Type, e.Message)
}

func (e Error) Error() string {
	return e.String()
}

type CDCEvent struct {
	Id     string `json:"eventId" bson:"_id,omitempty"`
	Ts     int64  `json:"ts" bson:"ts"` // Unix nanoseconds
	Op     string `json:"op" bson:"op"` // i=insert, u=update, d=delete
	Caller string `json:"user" bson:"caller"`

	EntityId   string  `json:"entityId" bson:"entityId"`           // _id of entity
	EntityType string  `json:"entityType" bson:"entityType"`       // user-defined
	EntityRev  int64   `json:"rev" bson:"entityRev"`               // entity revision as of this op, 0 on insert
	Old        *Entity `json:"old,omitempty" bson:"old,omitempty"` // old values of affected labels, null on insert
	New        *Entity `json:"new,omitempty" bson:"new,omitempty"` // new values of affected labels, null on delete

	// Set op fields are optional, copied from entity if set. The three
	// fields are all or nothing: all should be set, or none should be set.
	// Etre has no semantic awareness of set op values, nor does it validate
	// them. The caller is responsible for ensuring they're correct.
	SetId   string `json:"setId,omitempty" bson:"setId,omitempty"`
	SetOp   string `json:"setOp,omitempty" bson:"setOp,omitempty"`
	SetSize int    `json:"setSize,omitempty" bson:"setSize,omitempty"`
}

// Latency represents network latencies in milliseconds.
type Latency struct {
	Send int64 // client -> server
	Recv int64 // server -> client
	RTT  int64 // client -> server -> client
}

var (
	DebugEnabled = false
	debugLog     = log.New(os.Stderr, "DEBUG ", log.LstdFlags|log.Lmicroseconds)
)

func Debug(msg string, v ...interface{}) {
	if !DebugEnabled {
		return
	}
	_, file, line, _ := runtime.Caller(1)
	msg = fmt.Sprintf("%s:%d %s", path.Base(file), line, msg)
	debugLog.Printf(msg, v...)
}
