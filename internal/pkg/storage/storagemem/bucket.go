package storagemem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/bufbuild/buf/internal/pkg/storage"
	"github.com/bufbuild/buf/internal/pkg/storage/storagepath"
	"go.uber.org/multierr"
)

type bucket struct {
	pathToBuffer map[string]*buffer
	closed       bool
	lock         sync.RWMutex
}

func newBucket() *bucket {
	return &bucket{
		pathToBuffer: make(map[string]*buffer),
	}
}

func (b *bucket) Type() string {
	return BucketType
}

func (b *bucket) Get(ctx context.Context, path string) (storage.ReadObject, error) {
	path, err := storagepath.NormalizeAndValidate(path)
	if err != nil {
		return nil, err
	}
	if path == "." {
		return nil, errors.New("cannot get root")
	}
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.closed {
		return nil, storage.ErrClosed
	}
	buffer, ok := b.pathToBuffer[path]
	if !ok {
		return nil, storage.NewErrNotExist(path)
	}
	size, err := buffer.Len()
	if err != nil {
		return nil, err
	}
	return newReadObject(buffer, uint32(size)), nil
}

func (b *bucket) Stat(ctx context.Context, path string) (storage.ObjectInfo, error) {
	path, err := storagepath.NormalizeAndValidate(path)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if path == "." {
		return storage.ObjectInfo{}, errors.New("cannot check root")
	}
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.closed {
		return storage.ObjectInfo{}, storage.ErrClosed
	}
	buffer, ok := b.pathToBuffer[path]
	if !ok {
		return storage.ObjectInfo{}, storage.NewErrNotExist(path)
	}
	size, err := buffer.Len()
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{
		Size: uint32(size),
	}, nil
}

func (b *bucket) Walk(ctx context.Context, prefix string, f func(string) error) error {
	prefix, err := storagepath.NormalizeAndValidate(prefix)
	if err != nil {
		return err
	}
	// without this, "internal/buf/proto" would call f for "internal/buf/protocompile"
	if prefix != "." {
		prefix = prefix + "/"
	}
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.closed {
		return storage.ErrClosed
	}
	fileCount := 0
	for path := range b.pathToBuffer {
		fileCount++
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if err == context.DeadlineExceeded {
				return fmt.Errorf("timed out after walking %d files: %v", fileCount, err)
			}
			return err
		default:
		}
		if prefix == "." || strings.HasPrefix(path, prefix) {
			// only normalized and validated paths can be put into the map
			if err := f(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *bucket) Put(ctx context.Context, path string, size uint32) (storage.WriteObject, error) {
	path, err := storagepath.NormalizeAndValidate(path)
	if err != nil {
		return nil, err
	}
	if path == "." {
		return nil, errors.New("cannot put root")
	}
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.closed {
		return nil, storage.ErrClosed
	}
	buffer, ok := b.pathToBuffer[path]
	if ok {
		// this has a deleted marker so that if we have outstanding
		// readers or writers, they will fail
		if err := buffer.MarkDeleted(); err != nil {
			return nil, err
		}
		// just in case
		delete(b.pathToBuffer, path)
	}
	buffer = newBuffer(size)
	b.pathToBuffer[path] = buffer
	return newWriteObject(buffer, size), nil
}

func (b *bucket) Close() error {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.closed {
		return storage.ErrClosed
	}
	var err error
	for _, buffer := range b.pathToBuffer {
		// this has a deleted marker so that if we have outstanding
		// readers or writers, they will fail
		err = multierr.Append(err, buffer.MarkDeleted())
	}
	// just in case we don't protect against close somewhere
	b.pathToBuffer = make(map[string]*buffer)
	b.closed = true
	return err
}

type readObject struct {
	buffer *buffer
	size   uint32
	read   int
	closed bool
	lock   sync.Mutex
}

func newReadObject(buffer *buffer, size uint32) *readObject {
	return &readObject{
		buffer: buffer,
		size:   size,
	}
}

func (r *readObject) Read(p []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return 0, storage.ErrClosed
	}
	if uint32(r.read) >= r.size {
		return 0, io.EOF
	}
	max := r.size - uint32(r.read)
	if max < uint32(len(p)) {
		p = p[:max]
	}
	n, err := r.buffer.CopyTo(p, r.read)
	r.read += n
	if uint32(r.read) >= r.size {
		err = io.EOF
	}
	return n, err
}

func (r *readObject) Close() error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return storage.ErrClosed
	}
	r.closed = true
	return nil
}

func (r *readObject) Size() uint32 {
	return r.size
}

type writeObject struct {
	buffer  *buffer
	size    uint32
	written int
	closed  bool
	lock    sync.Mutex
}

func newWriteObject(buffer *buffer, size uint32) *writeObject {
	return &writeObject{
		buffer: buffer,
		size:   size,
	}
}

func (r *writeObject) Write(p []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return 0, storage.ErrClosed
	}
	if uint32(r.written+len(p)) > r.size {
		return 0, io.EOF
	}
	n, err := r.buffer.CopyFrom(p, r.written)
	r.written += n
	return n, err
}

func (r *writeObject) Close() error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return storage.ErrClosed
	}
	r.closed = true
	if uint32(r.written) != r.size {
		return storage.ErrIncompleteWrite
	}
	return nil
}

func (r *writeObject) Size() uint32 {
	return r.size
}

type buffer struct {
	data   []byte
	curLen int
	// protect against outstanding readers or writers
	// if we overwrite a file
	deleted bool
	lock    sync.RWMutex
}

func newBuffer(size uint32) *buffer {
	return &buffer{
		data: make([]byte, int(size)),
	}
}

// CopyFrom copies from the byte slice to the buffer starting at the offset.
//
// Returns io.EOF if len(from) + offset is greater than the buffer size.
func (b *buffer) CopyFrom(from []byte, offset int) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.deleted {
		return 0, storage.ErrClosed
	}
	end := len(from) + offset
	if end > len(b.data) {
		return 0, io.EOF
	}
	copy(b.data[offset:end], from)
	if b.curLen < end {
		b.curLen = end
	}
	return len(from), nil
}

// CopyTo copies the from the buffer to the byte slice starting at the offset.
//
// Returns io.EOF if len(to) + offset is greater than Len().
func (b *buffer) CopyTo(to []byte, offset int) (int, error) {
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.deleted {
		return 0, storage.ErrClosed
	}
	end := len(to) + offset
	if end > b.curLen {
		return 0, io.EOF
	}
	copy(to, b.data[offset:end])
	return len(to), nil
}

// Len gets the current length.
func (b *buffer) Len() (int, error) {
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.deleted {
		return 0, storage.ErrClosed
	}
	return b.curLen, nil
}

// MarkDeleted marks the buffer as deleted.
func (b *buffer) MarkDeleted() error {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.deleted {
		return storage.ErrClosed
	}
	b.deleted = true
	return nil
}
