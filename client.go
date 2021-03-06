package sftp

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kr/fs"

	//"golang.org/x/crypto/ssh"
	"github.com/ScriptRock/crypto/ssh"
)

// MaxPacket sets the maximum size of the payload.
func MaxPacket(size int) func(*Client) error {
	return func(c *Client) error {
		if size < 1<<15 {
			return fmt.Errorf("size must be greater or equal to 32k")
		}
		c.maxPacket = size
		return nil
	}
}

// New creates a new SFTP client on conn.
func NewClient(conn *ssh.Client, opts ...func(*Client) error) (*Client, error) {
	s, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	if err := s.RequestSubsystem("sftp"); err != nil {
		return nil, err
	}
	pw, err := s.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, err := s.StdoutPipe()
	if err != nil {
		return nil, err
	}

	return NewClientPipe(pr, pw, opts...)
}

// NewClientPipe creates a new SFTP client given a Reader and a WriteCloser.
// This can be used for connecting to an SFTP server over TCP/TLS or by using
// the system's ssh client program (e.g. via exec.Command).
func NewClientPipe(rd io.Reader, wr io.WriteCloser, opts ...func(*Client) error) (*Client, error) {
	sftp := &Client{
		w:          wr,
		r:          rd,
		maxPacket:  1 << 15,
		inflight:   make(map[uint32]chan<- result),
		recvClosed: make(chan struct{}),
	}
	if err := sftp.applyOptions(opts...); err != nil {
		wr.Close()
		return nil, err
	}
	if err := sftp.sendInit(); err != nil {
		wr.Close()
		return nil, err
	}
	if err := sftp.recvVersion(); err != nil {
		wr.Close()
		return nil, err
	}
	go sftp.recv()
	return sftp, nil
}

// Client represents an SFTP session on a *ssh.ClientConn SSH connection.
// Multiple Clients can be active on a single SSH connection, and a Client
// may be called concurrently from multiple Goroutines.
//
// Client implements the github.com/kr/fs.FileSystem interface.
type Client struct {
	w io.WriteCloser
	r io.Reader

	maxPacket int // max packet size read or written.
	nextid    uint32

	mu         sync.Mutex               // ensures only on request is in flight to the server at once
	inflight   map[uint32]chan<- result // outstanding requests
	recvClosed chan struct{}            // remote end has closed the connection
}

// Close closes the SFTP session.
func (c *Client) Close() error {
	err := c.w.Close()
	<-c.recvClosed
	return err
}

// Create creates the named file mode 0666 (before umask), truncating it if
// it already exists. If successful, methods on the returned File can be
// used for I/O; the associated file descriptor has mode O_RDWR.
func (c *Client) Create(path string) (*File, error) {
	return c.open(path, flags(os.O_RDWR|os.O_CREATE|os.O_TRUNC))
}

const sftpProtocolVersion = 3 // http://tools.ietf.org/html/draft-ietf-secsh-filexfer-02

func (c *Client) sendInit() error {
	return sendPacket(c.w, sshFxInitPacket{
		Version: sftpProtocolVersion, // http://tools.ietf.org/html/draft-ietf-secsh-filexfer-02
	})
}

// returns the next value of c.nextid
func (c *Client) nextId() uint32 {
	return atomic.AddUint32(&c.nextid, 1)
}

func (c *Client) recvVersion() error {
	typ, data, err := recvPacket(c.r)
	if err != nil {
		return err
	}
	if typ != ssh_FXP_VERSION {
		return &unexpectedPacketErr{ssh_FXP_VERSION, typ}
	}

	version, _ := unmarshalUint32(data)
	if version != sftpProtocolVersion {
		return &unexpectedVersionErr{sftpProtocolVersion, version}
	}

	return nil
}

// broadcastErr sends an error to all goroutines waiting for a response.
func (c *Client) broadcastErr(err error) {
	c.mu.Lock()
	listeners := make([]chan<- result, 0, len(c.inflight))
	for _, ch := range c.inflight {
		listeners = append(listeners, ch)
	}
	c.mu.Unlock()
	for _, ch := range listeners {
		ch <- result{err: err}
	}
}

// recv continuously reads from the server and forwards responses to the
// appropriate channel.
func (c *Client) recv() {
	defer close(c.recvClosed)
	for {
		typ, data, err := recvPacket(c.r)
		if err != nil {
			// Return the error to all listeners.
			c.broadcastErr(err)
			return
		}
		sid, _ := unmarshalUint32(data)
		c.mu.Lock()
		ch, ok := c.inflight[sid]
		delete(c.inflight, sid)
		c.mu.Unlock()
		if !ok {
			// This is an unexpected occurrence. Send the error
			// back to all listeners so that they terminate
			// gracefully.
			c.broadcastErr(fmt.Errorf("sid: %v not fond", sid))
			return
		}
		ch <- result{typ: typ, data: data}
	}
}

// Walk returns a new Walker rooted at root.
func (c *Client) Walk(root string) *fs.Walker {
	return fs.WalkFS(root, c)
}

// ReadDir reads the directory named by dirname and returns a list of
// directory entries.
func (c *Client) ReadDir(p string) ([]os.FileInfo, error) {
	handle, err := c.opendir(p)
	if err != nil {
		return nil, err
	}
	defer c.close(handle) // this has to defer earlier than the lock below
	var attrs []os.FileInfo
	var done = false
	for !done {
		id := c.nextId()
		typ, data, err1 := c.sendRequest(sshFxpReaddirPacket{
			Id:     id,
			Handle: handle,
		})
		if err1 != nil {
			err = err1
			done = true
			break
		}
		switch typ {
		case ssh_FXP_NAME:
			sid, data := unmarshalUint32(data)
			if sid != id {
				return nil, &unexpectedIdErr{id, sid}
			}
			count, data := unmarshalUint32(data)
			for i := uint32(0); i < count; i++ {
				var filename string
				filename, data = unmarshalString(data)
				_, data = unmarshalString(data) // discard longname
				var attr *FileStat
				attr, data = unmarshalAttrs(data)
				if filename == "." || filename == ".." {
					continue
				}
				attrs = append(attrs, fileInfoFromStat(attr, path.Base(filename)))
			}
		case ssh_FXP_STATUS:
			// TODO(dfc) scope warning!
			err = eofOrErr(unmarshalStatus(id, data))
			done = true
		default:
			return nil, unimplementedPacketErr(typ)
		}
	}
	if err == io.EOF {
		err = nil
	}
	return attrs, err
}
func (c *Client) opendir(path string) (string, error) {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpOpendirPacket{
		Id:   id,
		Path: path,
	})
	if err != nil {
		return "", err
	}
	switch typ {
	case ssh_FXP_HANDLE:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return "", &unexpectedIdErr{id, sid}
		}
		handle, _ := unmarshalString(data)
		return handle, nil
	case ssh_FXP_STATUS:
		return "", unmarshalStatus(id, data)
	default:
		return "", unimplementedPacketErr(typ)
	}
}

func (c *Client) Lstat(p string) (os.FileInfo, error) {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpLstatPacket{
		Id:   id,
		Path: p,
	})
	if err != nil {
		return nil, err
	}
	switch typ {
	case ssh_FXP_ATTRS:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIdErr{id, sid}
		}
		attr, _ := unmarshalAttrs(data)
		return fileInfoFromStat(attr, path.Base(p)), nil
	case ssh_FXP_STATUS:
		return nil, unmarshalStatus(id, data)
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// ReadLink reads the target of a symbolic link.
func (c *Client) ReadLink(p string) (string, error) {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpReadlinkPacket{
		Id:   id,
		Path: p,
	})
	if err != nil {
		return "", err
	}
	switch typ {
	case ssh_FXP_NAME:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return "", &unexpectedIdErr{id, sid}
		}
		count, data := unmarshalUint32(data)
		if count != 1 {
			return "", unexpectedCount(1, count)
		}
		filename, _ := unmarshalString(data) // ignore dummy attributes
		return filename, nil
	case ssh_FXP_STATUS:
		return "", unmarshalStatus(id, data)
	default:
		return "", unimplementedPacketErr(typ)
	}
}

// setstat is a convience wrapper to allow for changing of various parts of the file descriptor.
func (c *Client) setstat(path string, flags uint32, attrs interface{}) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpSetstatPacket{
		Id:    id,
		Path:  path,
		Flags: flags,
		Attrs: attrs,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

// Chtimes changes the access and modification times of the named file.
func (c *Client) Chtimes(path string, atime time.Time, mtime time.Time) error {
	type times struct {
		Atime uint32
		Mtime uint32
	}
	attrs := times{uint32(atime.Unix()), uint32(mtime.Unix())}
	return c.setstat(path, ssh_FILEXFER_ATTR_ACMODTIME, attrs)
}

// Chown changes the user and group owners of the named file.
func (c *Client) Chown(path string, uid, gid int) error {
	type owner struct {
		Uid uint32
		Gid uint32
	}
	attrs := owner{uint32(uid), uint32(gid)}
	return c.setstat(path, ssh_FILEXFER_ATTR_UIDGID, attrs)
}

// Chmod changes the permissions of the named file.
func (c *Client) Chmod(path string, mode os.FileMode) error {
	return c.setstat(path, ssh_FILEXFER_ATTR_PERMISSIONS, uint32(mode))
}

// Truncate sets the size of the named file. Although it may be safely assumed
// that if the size is less than its current size it will be truncated to fit,
// the SFTP protocol does not specify what behavior the server should do when setting
// size greater than the current size.
func (c *Client) Truncate(path string, size int64) error {
	return c.setstat(path, ssh_FILEXFER_ATTR_SIZE, uint64(size))
}

// Open opens the named file for reading. If successful, methods on the
// returned file can be used for reading; the associated file descriptor
// has mode O_RDONLY.
func (c *Client) Open(path string) (*File, error) {
	return c.open(path, flags(os.O_RDONLY))
}

// OpenFile is the generalized open call; most users will use Open or
// Create instead. It opens the named file with specified flag (O_RDONLY
// etc.). If successful, methods on the returned File can be used for I/O.
func (c *Client) OpenFile(path string, f int) (*File, error) {
	return c.open(path, flags(f))
}

func (c *Client) open(path string, pflags uint32) (*File, error) {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpOpenPacket{
		Id:     id,
		Path:   path,
		Pflags: pflags,
	})
	if err != nil {
		return nil, err
	}
	switch typ {
	case ssh_FXP_HANDLE:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIdErr{id, sid}
		}
		handle, _ := unmarshalString(data)
		return &File{c: c, path: path, handle: handle}, nil
	case ssh_FXP_STATUS:
		return nil, unmarshalStatus(id, data)
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// close closes a handle handle previously returned in the response
// to SSH_FXP_OPEN or SSH_FXP_OPENDIR. The handle becomes invalid
// immediately after this request has been sent.
func (c *Client) close(handle string) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpClosePacket{
		Id:     id,
		Handle: handle,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

func (c *Client) fstat(handle string) (*FileStat, error) {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpFstatPacket{
		Id:     id,
		Handle: handle,
	})
	if err != nil {
		return nil, err
	}
	switch typ {
	case ssh_FXP_ATTRS:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIdErr{id, sid}
		}
		attr, _ := unmarshalAttrs(data)
		return attr, nil
	case ssh_FXP_STATUS:
		return nil, unmarshalStatus(id, data)
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// Get vfs stats from remote host.
// Implementing statvfs@openssh.com SSH_FXP_EXTENDED feature
// from http://www.opensource.apple.com/source/OpenSSH/OpenSSH-175/openssh/PROTOCOL?txt
func (c *Client) StatVFS(path string) (*StatVFS, error) {
	// send the StatVFS packet to the server
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpStatvfsPacket{
		Id:   id,
		Path: path,
	})
	if err != nil {
		return nil, err
	}

	switch typ {
	// server responded with valid data
	case ssh_FXP_EXTENDED_REPLY:
		var response StatVFS
		err = binary.Read(bytes.NewReader(data), binary.BigEndian, &response)
		if err != nil {
			return nil, errors.New("can not parse reply")
		}

		return &response, nil

	// the resquest failed
	case ssh_FXP_STATUS:
		return nil, errors.New(fxp(ssh_FXP_STATUS).String())

	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// Join joins any number of path elements into a single path, adding a
// separating slash if necessary. The result is Cleaned; in particular, all
// empty strings are ignored.
func (c *Client) Join(elem ...string) string { return path.Join(elem...) }

// Remove removes the specified file or directory. An error will be returned if no
// file or directory with the specified path exists, or if the specified directory
// is not empty.
func (c *Client) Remove(path string) error {
	err := c.removeFile(path)
	if status, ok := err.(*StatusError); ok && status.Code == ssh_FX_FAILURE {
		err = c.removeDirectory(path)
	}
	return err
}

func (c *Client) removeFile(path string) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpRemovePacket{
		Id:       id,
		Filename: path,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

func (c *Client) removeDirectory(path string) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpRmdirPacket{
		Id:   id,
		Path: path,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

// Rename renames a file.
func (c *Client) Rename(oldname, newname string) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpRenamePacket{
		Id:      id,
		Oldpath: oldname,
		Newpath: newname,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

// result captures the result of receiving the a packet from the server
type result struct {
	typ  byte
	data []byte
	err  error
}

type idmarshaler interface {
	id() uint32
	encoding.BinaryMarshaler
}

func (c *Client) sendRequest(p idmarshaler) (byte, []byte, error) {
	ch := make(chan result, 1)
	c.dispatchRequest(ch, p)
	s := <-ch
	return s.typ, s.data, s.err
}

func (c *Client) dispatchRequest(ch chan<- result, p idmarshaler) {
	c.mu.Lock()
	c.inflight[p.id()] = ch
	if err := sendPacket(c.w, p); err != nil {
		delete(c.inflight, p.id())
		c.mu.Unlock()
		ch <- result{err: err}
		return
	}
	c.mu.Unlock()
}

// Creates the specified directory. An error will be returned if a file or
// directory with the specified path already exists, or if the directory's
// parent folder does not exist (the method cannot create complete paths).
func (c *Client) Mkdir(path string) error {
	id := c.nextId()
	typ, data, err := c.sendRequest(sshFxpMkdirPacket{
		Id:   id,
		Path: path,
	})
	if err != nil {
		return err
	}
	switch typ {
	case ssh_FXP_STATUS:
		return okOrErr(unmarshalStatus(id, data))
	default:
		return unimplementedPacketErr(typ)
	}
}

// applyOptions applies options functions to the Client.
// If an error is encountered, option processing ceases.
func (c *Client) applyOptions(opts ...func(*Client) error) error {
	for _, f := range opts {
		if err := f(c); err != nil {
			return err
		}
	}
	return nil
}

// File represents a remote file.
type File struct {
	c      *Client
	path   string
	handle string
	offset uint64 // current offset within remote file
}

// Close closes the File, rendering it unusable for I/O. It returns an
// error, if any.
func (f *File) Close() error {
	return f.c.close(f.handle)
}

const maxConcurrentRequests = 64

// Read reads up to len(b) bytes from the File. It returns the number of
// bytes read and an error, if any. EOF is signaled by a zero count with
// err set to io.EOF.
func (f *File) Read(b []byte) (int, error) {
	// Split the read into multiple maxPacket sized concurrent reads
	// bounded by maxConcurrentRequests. This allows reads with a suitably
	// large buffer to transfer data at a much faster rate due to
	// overlapping round trip times.
	inFlight := 0
	desiredInFlight := 1
	offset := f.offset
	ch := make(chan result)
	type inflightRead struct {
		b      []byte
		offset uint64
	}
	reqs := map[uint32]inflightRead{}
	type offsetErr struct {
		offset uint64
		err    error
	}
	var firstErr offsetErr

	sendReq := func(b []byte, offset uint64) {
		reqId := f.c.nextId()
		f.c.dispatchRequest(ch, sshFxpReadPacket{
			Id:     reqId,
			Handle: f.handle,
			Offset: offset,
			Len:    uint32(len(b)),
		})
		inFlight++
		reqs[reqId] = inflightRead{b: b, offset: offset}
	}

	var read int
	for len(b) > 0 || inFlight > 0 {
		for inFlight < desiredInFlight && len(b) > 0 && firstErr.err == nil {
			l := min(len(b), f.c.maxPacket)
			rb := b[:l]
			sendReq(rb, offset)
			offset += uint64(l)
			b = b[l:]
		}

		if inFlight == 0 {
			break
		}
		select {
		case res := <-ch:
			inFlight--
			if res.err != nil {
				firstErr = offsetErr{offset: 0, err: res.err}
				break
			}
			reqId, data := unmarshalUint32(res.data)
			req, ok := reqs[reqId]
			if !ok {
				firstErr = offsetErr{offset: 0, err: fmt.Errorf("sid: %v not found", reqId)}
				break
			}
			delete(reqs, reqId)
			switch res.typ {
			case ssh_FXP_STATUS:
				if firstErr.err == nil || req.offset < firstErr.offset {
					firstErr = offsetErr{offset: req.offset, err: eofOrErr(unmarshalStatus(reqId, res.data))}
					break
				}
			case ssh_FXP_DATA:
				l, data := unmarshalUint32(data)
				n := copy(req.b, data[:l])
				read += n
				if n < len(req.b) {
					sendReq(req.b[l:], req.offset+uint64(l))
				}
				if desiredInFlight < maxConcurrentRequests {
					desiredInFlight++
				}
			default:
				firstErr = offsetErr{offset: 0, err: unimplementedPacketErr(res.typ)}
				break
			}
		}
	}
	// If the error is anything other than EOF, then there
	// may be gaps in the data copied to the buffer so it's
	// best to return 0 so the caller can't make any
	// incorrect assumptions about the state of the buffer.
	if firstErr.err != nil && firstErr.err != io.EOF {
		read = 0
	}
	f.offset += uint64(read)
	return read, firstErr.err
}

// WriteTo writes the file to w. The return value is the number of bytes
// written. Any error encountered during the write is also returned.
func (f *File) WriteTo(w io.Writer) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	inFlight := 0
	desiredInFlight := 1
	offset := f.offset
	writeOffset := offset
	fileSize := uint64(fi.Size())
	ch := make(chan result)
	type inflightRead struct {
		b      []byte
		offset uint64
	}
	reqs := map[uint32]inflightRead{}
	pendingWrites := map[uint64][]byte{}
	type offsetErr struct {
		offset uint64
		err    error
	}
	var firstErr offsetErr

	sendReq := func(b []byte, offset uint64) {
		reqId := f.c.nextId()
		f.c.dispatchRequest(ch, sshFxpReadPacket{
			Id:     reqId,
			Handle: f.handle,
			Offset: offset,
			Len:    uint32(len(b)),
		})
		inFlight++
		reqs[reqId] = inflightRead{b: b, offset: offset}
	}

	var copied int64
	for firstErr.err == nil || inFlight > 0 {
		for inFlight < desiredInFlight && firstErr.err == nil {
			b := make([]byte, f.c.maxPacket)
			sendReq(b, offset)
			offset += uint64(f.c.maxPacket)
			if offset > fileSize {
				desiredInFlight = 1
			}
		}

		if inFlight == 0 {
			break
		}
		select {
		case res := <-ch:
			inFlight--
			if res.err != nil {
				firstErr = offsetErr{offset: 0, err: res.err}
				break
			}
			reqId, data := unmarshalUint32(res.data)
			req, ok := reqs[reqId]
			if !ok {
				firstErr = offsetErr{offset: 0, err: fmt.Errorf("sid: %v not found", reqId)}
				break
			}
			delete(reqs, reqId)
			switch res.typ {
			case ssh_FXP_STATUS:
				if firstErr.err == nil || req.offset < firstErr.offset {
					firstErr = offsetErr{offset: req.offset, err: eofOrErr(unmarshalStatus(reqId, res.data))}
					break
				}
			case ssh_FXP_DATA:
				l, data := unmarshalUint32(data)
				if req.offset == writeOffset {
					nbytes, err := w.Write(data)
					copied += int64(nbytes)
					if err != nil {
						firstErr = offsetErr{offset: req.offset + uint64(nbytes), err: err}
						break
					}
					if nbytes < int(l) {
						firstErr = offsetErr{offset: req.offset + uint64(nbytes), err: io.ErrShortWrite}
						break
					}
					switch {
					case offset > fileSize:
						desiredInFlight = 1
					case desiredInFlight < maxConcurrentRequests:
						desiredInFlight++
					}
					writeOffset += uint64(nbytes)
					for pendingData, ok := pendingWrites[writeOffset]; ok; pendingData, ok = pendingWrites[writeOffset] {
						nbytes, err := w.Write(pendingData)
						if err != nil {
							firstErr = offsetErr{offset: writeOffset + uint64(nbytes), err: err}
							break
						}
						if nbytes < len(pendingData) {
							firstErr = offsetErr{offset: writeOffset + uint64(nbytes), err: io.ErrShortWrite}
							break
						}
						writeOffset += uint64(nbytes)
						inFlight--
					}
				} else {
					// Don't write the data yet because
					// this response came in out of order
					// and we need to wait for responses
					// for earlier segments of the file.
					inFlight++ // Pending writes should still be considered inFlight.
					pendingWrites[req.offset] = data
				}
			default:
				firstErr = offsetErr{offset: 0, err: unimplementedPacketErr(res.typ)}
				break
			}
		}
	}
	if firstErr.err != io.EOF {
		return copied, firstErr.err
	}
	return copied, nil

}

// Stat returns the FileInfo structure describing file. If there is an
// error.
func (f *File) Stat() (os.FileInfo, error) {
	fs, err := f.c.fstat(f.handle)
	if err != nil {
		return nil, err
	}
	return fileInfoFromStat(fs, path.Base(f.path)), nil
}

// Write writes len(b) bytes to the File. It returns the number of bytes
// written and an error, if any. Write returns a non-nil error when n !=
// len(b).
func (f *File) Write(b []byte) (int, error) {
	// Split the write into multiple maxPacket sized concurrent writes
	// bounded by maxConcurrentRequests. This allows writes with a suitably
	// large buffer to transfer data at a much faster rate due to
	// overlapping round trip times.
	inFlight := 0
	desiredInFlight := 1
	offset := f.offset
	ch := make(chan result)
	var firstErr error
	written := len(b)
	for len(b) > 0 || inFlight > 0 {
		for inFlight < desiredInFlight && len(b) > 0 && firstErr == nil {
			l := min(len(b), f.c.maxPacket)
			rb := b[:l]
			f.c.dispatchRequest(ch, sshFxpWritePacket{
				Id:     f.c.nextId(),
				Handle: f.handle,
				Offset: offset,
				Length: uint32(len(rb)),
				Data:   rb,
			})
			inFlight++
			offset += uint64(l)
			b = b[l:]
		}

		if inFlight == 0 {
			break
		}
		select {
		case res := <-ch:
			inFlight--
			if res.err != nil {
				firstErr = res.err
				break
			}
			switch res.typ {
			case ssh_FXP_STATUS:
				id, _ := unmarshalUint32(res.data)
				err := okOrErr(unmarshalStatus(id, res.data))
				if err != nil && firstErr == nil {
					firstErr = err
					break
				}
				if desiredInFlight < maxConcurrentRequests {
					desiredInFlight++
				}
			default:
				firstErr = unimplementedPacketErr(res.typ)
				break
			}
		}
	}
	// If error is non-nil, then there may be gaps in the data written to
	// the file so it's best to return 0 so the caller can't make any
	// incorrect assumptions about the state of the file.
	if firstErr != nil {
		written = 0
	}
	f.offset += uint64(written)
	return written, firstErr
}

// ReadFrom reads data from r until EOF and writes it to the file. The return
// value is the number of bytes read. Any error except io.EOF encountered
// during the read is also returned.
func (f *File) ReadFrom(r io.Reader) (int64, error) {
	inFlight := 0
	desiredInFlight := 1
	offset := f.offset
	ch := make(chan result)
	var firstErr error
	read := int64(0)
	b := make([]byte, f.c.maxPacket)
	for inFlight > 0 || firstErr == nil {
		for inFlight < desiredInFlight && firstErr == nil {
			n, err := r.Read(b)
			if err != nil {
				firstErr = err
			}
			f.c.dispatchRequest(ch, sshFxpWritePacket{
				Id:     f.c.nextId(),
				Handle: f.handle,
				Offset: offset,
				Length: uint32(n),
				Data:   b[:n],
			})
			inFlight++
			offset += uint64(n)
			read += int64(n)
		}

		if inFlight == 0 {
			break
		}
		select {
		case res := <-ch:
			inFlight--
			if res.err != nil {
				firstErr = res.err
				break
			}
			switch res.typ {
			case ssh_FXP_STATUS:
				id, _ := unmarshalUint32(res.data)
				err := okOrErr(unmarshalStatus(id, res.data))
				if err != nil && firstErr == nil {
					firstErr = err
					break
				}
				if desiredInFlight < maxConcurrentRequests {
					desiredInFlight++
				}
			default:
				firstErr = unimplementedPacketErr(res.typ)
				break
			}
		}
	}
	if firstErr == io.EOF {
		firstErr = nil
	}
	// If error is non-nil, then there may be gaps in the data written to
	// the file so it's best to return 0 so the caller can't make any
	// incorrect assumptions about the state of the file.
	if firstErr != nil {
		read = 0
	}
	f.offset += uint64(read)
	return read, firstErr
}

// Seek implements io.Seeker by setting the client offset for the next Read or
// Write. It returns the next offset read. Seeking before or after the end of
// the file is undefined. Seeking relative to the end calls Stat.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case os.SEEK_SET:
		f.offset = uint64(offset)
	case os.SEEK_CUR:
		f.offset = uint64(int64(f.offset) + offset)
	case os.SEEK_END:
		fi, err := f.Stat()
		if err != nil {
			return int64(f.offset), err
		}
		f.offset = uint64(fi.Size() + offset)
	default:
		return int64(f.offset), unimplementedSeekWhence(whence)
	}
	return int64(f.offset), nil
}

// Chown changes the uid/gid of the current file.
func (f *File) Chown(uid, gid int) error {
	return f.c.Chown(f.path, uid, gid)
}

// Chmod changes the permissions of the current file.
func (f *File) Chmod(mode os.FileMode) error {
	return f.c.Chmod(f.path, mode)
}

// Truncate sets the size of the current file. Although it may be safely assumed
// that if the size is less than its current size it will be truncated to fit,
// the SFTP protocol does not specify what behavior the server should do when setting
// size greater than the current size.
func (f *File) Truncate(size int64) error {
	return f.c.Truncate(f.path, size)
}

func min(a, b int) int {
	if a > b {
		return b
	}
	return a
}

// okOrErr returns nil if Err.Code is SSH_FX_OK, otherwise it returns the error.
func okOrErr(err error) error {
	if err, ok := err.(*StatusError); ok && err.Code == ssh_FX_OK {
		return nil
	}
	return err
}

func eofOrErr(err error) error {
	if err, ok := err.(*StatusError); ok && err.Code == ssh_FX_EOF {
		return io.EOF
	}
	return err
}

func unmarshalStatus(id uint32, data []byte) error {
	sid, data := unmarshalUint32(data)
	if sid != id {
		return &unexpectedIdErr{id, sid}
	}
	code, data := unmarshalUint32(data)
	msg, data := unmarshalString(data)
	lang, _ := unmarshalString(data)
	return &StatusError{
		Code: code,
		msg:  msg,
		lang: lang,
	}
}

// flags converts the flags passed to OpenFile into ssh flags.
// Unsupported flags are ignored.
func flags(f int) uint32 {
	var out uint32
	switch f & os.O_WRONLY {
	case os.O_WRONLY:
		out |= ssh_FXF_WRITE
	case os.O_RDONLY:
		out |= ssh_FXF_READ
	}
	if f&os.O_RDWR == os.O_RDWR {
		out |= ssh_FXF_READ | ssh_FXF_WRITE
	}
	if f&os.O_APPEND == os.O_APPEND {
		out |= ssh_FXF_APPEND
	}
	if f&os.O_CREATE == os.O_CREATE {
		out |= ssh_FXF_CREAT
	}
	if f&os.O_TRUNC == os.O_TRUNC {
		out |= ssh_FXF_TRUNC
	}
	if f&os.O_EXCL == os.O_EXCL {
		out |= ssh_FXF_EXCL
	}
	return out
}
