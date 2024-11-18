package cache

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

type Item struct {
	Object     any
	Expiration int64
}

// Expired Returns true if the item has expired.
func (item Item) Expired() bool {
	return item.expired(time.Now().UnixNano())
}

func (item Item) expired(now int64) bool {
	return item.Expiration > 0 && now > item.Expiration
}

const (
	// NoExpiration For use with functions that take an expiration time.
	NoExpiration time.Duration = -1
	// DefaultExpiration used with functions that take an expiration time. Equivalent to
	// passing in the same expiration duration as was given to New() or
	// NewFrom() when the cache was created (e.g. 5 minutes.)
	DefaultExpiration time.Duration = 0
)

// Cache main working cache structure
type Cache struct {
	*cache
	// If this is confusing, see the comment at the bottom of New()
}

type cache struct {
	defaultExpiration time.Duration
	items             sync.Map
	onEvicted         func(string, any)
	timeCache         atomic.Int64
	stopped           chan any
}

// Set Adds an item to the cache, replacing any existing item. If the duration is 0
// (DefaultExpiration), the cache's default expiration time is used. If it is -1
// (NoExpiration), the item never expires.
func (c *cache) Set(k string, x any, d time.Duration) {
	c.set(k, x, d)
}

func (c *cache) set(k string, x any, d time.Duration) {
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = c.timeCache.Load() + d.Nanoseconds()
	}
	c.items.Store(k, Item{
		Object:     x,
		Expiration: e,
	})
}

// SetDefault Adds an item to the cache, replacing any existing item, using the default
// expiration.
func (c *cache) SetDefault(k string, x any) {
	c.set(k, x, DefaultExpiration)
}

var (
	ErrAlreadyExists = errors.New("key already exists")
	ErrNotExists     = errors.New("key not exists")
)

// Add an item to the cache only if an item doesn't already exist for the given
// key, or if the existing item has expired. Returns an error otherwise.
func (c *cache) Add(k string, x any, d time.Duration) error {
	_, found := c.get(k)
	if found {
		return ErrAlreadyExists
	}
	c.set(k, x, d)
	return nil
}

// Replace Sets a new value for the cache key only if it already exists, and the existing
// item hasn't expired. Returns an error otherwise.
func (c *cache) Replace(k string, x any, d time.Duration) error {
	_, found := c.get(k)
	if !found {
		return ErrNotExists
	}
	c.set(k, x, d)
	return nil
}

// Get an item from the cache. Returns the item or nil, and a bool indicating
// whether the key was found.
func (c *cache) Get(k string) (any, bool) {
	return c.get(k)
}

// GetWithExpiration returns an item and its expiration time from the cache.
// It returns the item or nil, the expiration time if one is set (if the item
// never expires a zero value for time.Time is returned), and a bool indicating
// whether the key was found.
func (c *cache) GetWithExpiration(k string) (any, time.Time, bool) {
	tmp, found := c.items.Load(k)
	if !found {
		return nil, time.Time{}, false
	}
	item := tmp.(Item)
	if item.Expiration > 0 {
		if c.timeCache.Load() > item.Expiration {
			return nil, time.Time{}, false
		}

		// Return the item and the expiration time
		return item.Object, time.Unix(0, item.Expiration), true
	}

	// If expiration <= 0 (i.e. no expiration time set) then return the item
	// and a zeroed time.Time
	return item.Object, time.Time{}, true
}

// GetWithTTL same as GetWithExpiration, but returns time.Duration before value expired.
func (c *cache) GetWithTTL(k string) (v any, ttl time.Duration, found bool) {
	var exp time.Time
	if v, exp, found = c.GetWithExpiration(k); found {
		ttl = time.Unix(0, c.timeCache.Load()).Sub(exp)
	}
	return
}

func (c *cache) get(k string) (any, bool) {
	tmp, found := c.items.Load(k)
	if !found {
		return nil, false
	}
	item := tmp.(Item)
	if item.expired(c.timeCache.Load()) {
		return nil, false
	}
	return item.Object, true
}

func (c *cache) getItem(k string) (Item, bool) {
	tmp, found := c.items.Load(k)
	if !found {
		return Item{}, false
	}
	return tmp.(Item), true
}

var ErrInvalidType = errors.New("incompatible value type")

// Increment an item of type int, int8, int16, int32, int64, uintptr, uint,
// uint8, uint32, or uint64, float32 or float64 by n. Returns an error if the
// item's value is not an integer, if it was not found, or if it is not
// possible to increment it by n. To retrieve the incremented value, use one
// of the specialized methods, e.g. IncrementInt64.
func (c *cache) Increment(k string, n int64) error {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return ErrNotExists
	}
	switch v.Object.(type) {
	case int:
		v.Object = v.Object.(int) + int(n)
	case int8:
		v.Object = v.Object.(int8) + int8(n)
	case int16:
		v.Object = v.Object.(int16) + int16(n)
	case int32:
		v.Object = v.Object.(int32) + int32(n)
	case int64:
		v.Object = v.Object.(int64) + n
	case uint:
		v.Object = v.Object.(uint) + uint(n)
	case uintptr:
		v.Object = v.Object.(uintptr) + uintptr(n)
	case uint8:
		v.Object = v.Object.(uint8) + uint8(n)
	case uint16:
		v.Object = v.Object.(uint16) + uint16(n)
	case uint32:
		v.Object = v.Object.(uint32) + uint32(n)
	case uint64:
		v.Object = v.Object.(uint64) + uint64(n)
	case float32:
		v.Object = v.Object.(float32) + float32(n)
	case float64:
		v.Object = v.Object.(float64) + float64(n)
	default:
		return ErrInvalidType
	}
	c.items.Store(k, v)
	return nil
}

// IncrementFloat Increments an item of type float32 or float64 by n. Returns an error if the
// item's value is not floating point, if it was not found, or if it is not
// possible to increment it by n. Pass a negative number to decrement the
// value. To retrieve the incremented value, use one of the specialized methods,
// e.g. IncrementFloat64.
func (c *cache) IncrementFloat(k string, n float64) error {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return ErrNotExists
	}
	switch v.Object.(type) {
	case float32:
		v.Object = v.Object.(float32) + float32(n)
	case float64:
		v.Object = v.Object.(float64) + n
	default:
		return ErrInvalidType
	}
	c.items.Store(k, v)
	return nil
}

// IncrementInt Increments an item of type int by n. Returns an error if the item's value is
// not an int, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementInt(k string, n int) (int, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementInt8 Increments an item of type int8 by n. Returns an error if the item's value is
// not an int8, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementInt8(k string, n int8) (int8, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int8)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementInt16 Increments an item of type int16 by n. Returns an error if the item's value is
// not an int16, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementInt16(k string, n int16) (int16, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int16)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementInt32 Increments an item of type int32 by n. Returns an error if the item's value is
// not an int32, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementInt32(k string, n int32) (int32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementInt64 Increments an item of type int64 by n. Returns an error if the item's value is
// not an int64, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementInt64(k string, n int64) (int64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUint Increments an item of type uint by n. Returns an error if the item's value is
// not an uint, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache) IncrementUint(k string, n uint) (uint, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUintptr Increments an item of type uintptr by n. Returns an error if the item's value
// is not an uintptr, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementUintptr(k string, n uintptr) (uintptr, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uintptr)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUint8 Increments an item of type uint8 by n. Returns an error if the item's value
// is not an uint8, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementUint8(k string, n uint8) (uint8, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint8)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUint16 Increments an item of type uint16 by n. Returns an error if the item's value
// is not an uint16, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementUint16(k string, n uint16) (uint16, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint16)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUint32 Increments an item of type uint32 by n. Returns an error if the item's value
// is not an uint32, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementUint32(k string, n uint32) (uint32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementUint64 Increments an item of type uint64 by n. Returns an error if the item's value
// is not an uint64, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementUint64(k string, n uint64) (uint64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementFloat32 Increments an item of type float32 by n. Returns an error if the item's value
// is not an float32, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementFloat32(k string, n float32) (float32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(float32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// IncrementFloat64 Increments an item of type float64 by n. Returns an error if the item's value
// is not an float64, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache) IncrementFloat64(k string, n float64) (float64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(float64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv + n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// Decrement an item of type int, int8, int16, int32, int64, uintptr, uint,
// uint8, uint32, or uint64, float32 or float64 by n. Returns an error if the
// item's value is not an integer, if it was not found, or if it is not
// possible to decrement it by n. To retrieve the decremented value, use one
// of the specialized methods, e.g. DecrementInt64.
func (c *cache) Decrement(k string, n int64) error {
	// TODO: Implement Increment and Decrement more cleanly.
	// (Cannot do Increment(k, n*-1) for uints.)
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return ErrNotExists
	}
	switch v.Object.(type) {
	case int:
		v.Object = v.Object.(int) - int(n)
	case int8:
		v.Object = v.Object.(int8) - int8(n)
	case int16:
		v.Object = v.Object.(int16) - int16(n)
	case int32:
		v.Object = v.Object.(int32) - int32(n)
	case int64:
		v.Object = v.Object.(int64) - n
	case uint:
		v.Object = v.Object.(uint) - uint(n)
	case uintptr:
		v.Object = v.Object.(uintptr) - uintptr(n)
	case uint8:
		v.Object = v.Object.(uint8) - uint8(n)
	case uint16:
		v.Object = v.Object.(uint16) - uint16(n)
	case uint32:
		v.Object = v.Object.(uint32) - uint32(n)
	case uint64:
		v.Object = v.Object.(uint64) - uint64(n)
	case float32:
		v.Object = v.Object.(float32) - float32(n)
	case float64:
		v.Object = v.Object.(float64) - float64(n)
	default:
		return ErrInvalidType
	}
	c.items.Store(k, v)
	return nil
}

// DecrementFloat Decrements an item of type float32 or float64 by n. Returns an error if the
// item's value is not floating point, if it was not found, or if it is not
// possible to decrement it by n. Pass a negative number to decrement the
// value. To retrieve the decremented value, use one of the specialized methods,
// e.g. DecrementFloat64.
func (c *cache) DecrementFloat(k string, n float64) error {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return ErrNotExists
	}
	switch v.Object.(type) {
	case float32:
		v.Object = v.Object.(float32) - float32(n)
	case float64:
		v.Object = v.Object.(float64) - n
	default:
		return ErrInvalidType
	}
	c.items.Store(k, v)
	return nil
}

// DecrementInt Decrements an item of type int by n. Returns an error if the item's value is
// not an int, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementInt(k string, n int) (int, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementInt8 Decrements an item of type int8 by n. Returns an error if the item's value is
// not an int8, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementInt8(k string, n int8) (int8, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int8)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementInt16 Decrements an item of type int16 by n. Returns an error if the item's value is
// not an int16, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementInt16(k string, n int16) (int16, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int16)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementInt32 Decrements an item of type int32 by n. Returns an error if the item's value is
// not an int32, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementInt32(k string, n int32) (int32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementInt64 Decrements an item of type int64 by n. Returns an error if the item's value is
// not an int64, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementInt64(k string, n int64) (int64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(int64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUint Decrements an item of type uint by n. Returns an error if the item's value is
// not an uint, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementUint(k string, n uint) (uint, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUintptr Decrements an item of type uintptr by n. Returns an error if the item's value
// is not an uintptr, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementUintptr(k string, n uintptr) (uintptr, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uintptr)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUint8 Decrements an item of type uint8 by n. Returns an error if the item's value is
// not an uint8, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache) DecrementUint8(k string, n uint8) (uint8, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint8)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUint16 Decrements an item of type uint16 by n. Returns an error if the item's value
// is not an uint16, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementUint16(k string, n uint16) (uint16, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint16)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUint32 Decrements an item of type uint32 by n. Returns an error if the item's value
// is not an uint32, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementUint32(k string, n uint32) (uint32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementUint64 Decrements an item of type uint64 by n. Returns an error if the item's value
// is not an uint64, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementUint64(k string, n uint64) (uint64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(uint64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementFloat32 Decrements an item of type float32 by n. Returns an error if the item's value
// is not an float32, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementFloat32(k string, n float32) (float32, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(float32)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// DecrementFloat64 Decrements an item of type float64 by n. Returns an error if the item's value
// is not an float64, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache) DecrementFloat64(k string, n float64) (float64, error) {
	v, found := c.getItem(k)
	if !found || v.expired(c.timeCache.Load()) {
		return 0, ErrNotExists
	}
	rv, ok := v.Object.(float64)
	if !ok {
		return 0, ErrInvalidType
	}
	nv := rv - n
	v.Object = nv
	c.items.Store(k, v)
	return nv, nil
}

// Delete an item from the cache. Does nothing if the key is not in the cache.
func (c *cache) Delete(k string) {
	if v, evicted := c.delete(k); evicted {
		c.onEvicted(k, v)
	}
}

func (c *cache) delete(k string) (any, bool) {
	if c.onEvicted != nil {
		tmp, found := c.items.Load(k)
		v := tmp.(Item)
		if found {
			c.items.Delete(k)
			return v.Object, true
		}
	}
	c.items.Delete(k)
	return nil, false
}

type kv struct {
	key   string
	value any
}

// DeleteExpired Deletes all expired items from the cache.
func (c *cache) DeleteExpired() {
	var evictedItems []kv
	now := c.timeCache.Load()
	c.items.Range(func(key, value any) bool {
		v := value.(Item)
		k := key.(string)
		if v.expired(now) {
			if ov, evicted := c.delete(k); evicted {
				evictedItems = append(evictedItems, kv{k, ov})
			}
		}
		return true // if false, Range stops
	})

	for _, v := range evictedItems {
		c.onEvicted(v.key, v.value)
	}
}

func (c *cache) deleteExpired(now int64) {
	var evictedItems []kv
	c.items.Range(func(key, value any) bool {
		v := value.(Item)
		k := key.(string)
		if v.expired(now) {
			if ov, evicted := c.delete(k); evicted {
				evictedItems = append(evictedItems, kv{k, ov})
			}
		}
		return true // if false, Range stops
	})

	for _, v := range evictedItems {
		c.onEvicted(v.key, v.value)
	}
}

// OnEvicted Sets an (optional) function that is called with the key and value when an
// item is evicted from the cache. (Including when it is deleted manually, but
// not when it is overwritten.) Set to nil to disable.
func (c *cache) OnEvicted(f func(string, any)) {
	c.onEvicted = f
}

// Save Writes the cache's items (using Gob) to an io.Writer.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache) Save(w io.Writer) (err error) {
	m := make(map[string]Item)
	c.items.Range(func(key, value any) bool {
		v := value.(Item)
		k := key.(string)
		m[k] = v
		return true // if false, Range stops
	})

	enc := gob.NewEncoder(w)
	defer func() {
		if x := recover(); x != nil {
			switch a := x.(type) {
			case string:
				err = errors.New("unable to register item type with Gob: " + a)
			case fmt.Stringer:
				err = errors.New("unable to register item type with Gob: " + a.String())
			case error:
				err = a
			default:
				err = errors.New("unable to register item type with Gob for undefined reason")
				debug.PrintStack()
			}
		}
	}()
	for _, v := range m {
		gob.Register(v.Object)
	}
	err = enc.Encode(m)
	return
}

// SaveFile Saves the cache's items to the given filename, creating the file if it
// doesn't exist, and overwriting it if it does.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache) SaveFile(fname string) error {
	fp, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer fp.Close()
	err = c.Save(fp)
	if err != nil {
		return err
	}
	return nil
}

// Load Adds (Gob-serialized) cache items from an io.Reader, excluding any items with
// keys that already exist (and haven't expired) in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache) Load(r io.Reader) error {
	dec := gob.NewDecoder(r)
	items := map[string]Item{}
	err := dec.Decode(&items)
	if err == nil {
		for k, v := range items {
			ov, found := c.getItem(k)
			if !found || ov.expired(c.timeCache.Load()) {
				c.items.Store(k, v)
			}
		}
	}
	return err
}

// LoadFile Loads and add cache items from the given filename, excluding any items with
// keys that already exist in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache) LoadFile(fname string) error {
	fp, err := os.Open(fname)
	if err != nil {
		return err
	}
	defer fp.Close()
	err = c.Load(fp)
	if err != nil {
		return err
	}
	return nil
}

// Items Copies all unexpired items in the cache into a new map and returns it.
func (c *cache) Items() map[string]Item {
	m := make(map[string]Item)
	now := c.timeCache.Load()
	c.items.Range(func(key, value any) bool {
		v := value.(Item)
		k := key.(string)
		if !v.expired(now) {
			m[k] = v
		}
		return true // if false, Range stops
	})
	return m
}

// ItemCount Returns the number of items in the cache. This may include items that have
// expired, but have not yet been cleaned up.
func (c *cache) ItemCount() int {
	n := 0
	c.items.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// Flush Deletes all items from the cache.
func (c *cache) Flush() {
	c.items.Clear()
}

func stopBackground(c *Cache) {
	c.stopped <- true
}

func startBackground(c *cache, cleanInterval time.Duration, preciseTime bool) {
	if cleanInterval > 0 {
		go func() {
			cleanTicker := time.NewTicker(cleanInterval)
			for {
				select {
				case now := <-cleanTicker.C:
					c.deleteExpired(now.UnixNano())
				case <-c.stopped:
					cleanTicker.Stop()
					return
				}
			}
		}()
	}

	go func() {
		var timeCacheInterval time.Duration
		if preciseTime {
			timeCacheInterval = time.Millisecond
		} else {
			timeCacheInterval = time.Second
		}
		timeTicker := time.NewTicker(timeCacheInterval)
		for {
			select {
			case now := <-timeTicker.C:
				c.timeCache.Store(now.UnixNano())
			case <-c.stopped:
				timeTicker.Stop()
				return
			}
		}
	}()
}

func newCacheWithJanitor(de time.Duration, ci time.Duration, preciseTime bool) *Cache {
	if de == 0 {
		de = -1
	}
	c := &cache{
		defaultExpiration: de,
		items:             sync.Map{},
		stopped:           make(chan any, 2),
	}
	c.timeCache.Store(time.Now().UnixNano())
	// This trick ensures that the janitor goroutine (which--granted it
	// was enabled--is running DeleteExpired on c forever) does not keep
	// the returned C object from being garbage collected. When it is
	// garbage collected, the finalizer stops the janitor goroutine, after
	// which c can be collected.
	C := &Cache{c}
	if ci == 0 {
		ci = math.MaxInt64
	}
	startBackground(c, ci, preciseTime)
	runtime.SetFinalizer(C, stopBackground)
	return C
}

// New Returns a new cache with a given default expiration duration and cleanup
// interval. If the expiration duration is less than one (or NoExpiration),
// the items in the cache never expire (by default), and must be deleted
// manually. If the cleanup interval is less than one, expired items are not
// deleted from the cache before calling c.DeleteExpired().
//
// By default entry expiration is rounded to 1s to decrease time.Now() calls,
// if preciseTime set to true, expiration will be rounded to 1ms.
func New(defaultExpiration, cleanupInterval time.Duration, preciseTime ...bool) *Cache {
	return newCacheWithJanitor(defaultExpiration, cleanupInterval, len(preciseTime) > 0 && preciseTime[0])
}

// NewFrom Returns a new cache with a given default expiration duration and cleanup
// interval. If the expiration duration is less than one (or NoExpiration),
// the items in the cache never expire (by default), and must be deleted
// manually. If the cleanup interval is less than one, expired items are not
// deleted from the cache before calling c.DeleteExpired().
//
// NewFrom() also accepts an items map which will serve as the underlying map
// for the cache. This is useful for starting from a deserialized cache
// (serialized using e.g. gob.Encode() on c.Items()), or passing in e.g.
// make(map[string]Item, 500) to improve startup performance when the cache
// is expected to reach a certain minimum size.
//
// Only the cache's methods synchronize access to this map, so it is not
// recommended to keep any references to the map around after creating a cache.
// If need be, the map can be accessed at a later point using c.Items() (subject
// to the same caveat.)
//
// By default entry expiration is rounded to 1s to decrease time.Now() calls,
// if preciseTime set to true, expiration will be rounded to 1ms.
//
// Note regarding serialization: When using e.g. gob, make sure to
// gob.Register() the individual types stored in the cache before encoding a
// map retrieved with c.Items(), and to register those same types before
// decoding a blob containing an items map.
func NewFrom(defaultExpiration, cleanupInterval time.Duration, items map[string]Item, preciseTime ...bool) *Cache {
	c := newCacheWithJanitor(defaultExpiration, cleanupInterval, len(preciseTime) > 0 && preciseTime[0])
	for k, v := range items {
		c.items.Store(k, v)
	}
	return c
}
