package fuse

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/superfly/litefs"
)

var _ fuse.RawFileSystem = (*FileSystem)(nil)
var _ litefs.InodeNotifier = (*FileSystem)(nil)

// FileSystem represents a raw interface to the FUSE file system.
type FileSystem struct {
	mu     sync.Mutex
	path   string // mount path
	server *fuse.Server
	store  *litefs.Store

	// Manage file handle creation.
	nextHandleID uint64
	fileHandles  map[uint64]*FileHandle
	dirHandles   map[uint64]*DirHandle

	// User ID for all files in the filesystem.
	Uid int

	// Group ID for all files in the filesystem.
	Gid int

	// If true, logs debug information about every FUSE call.
	Debug bool
}

// NewFileSystem returns a new instance of FileSystem.
func NewFileSystem(path string, store *litefs.Store) *FileSystem {
	return &FileSystem{
		path:  path,
		store: store,

		nextHandleID: 0xff00,
		fileHandles:  make(map[uint64]*FileHandle),
		dirHandles:   make(map[uint64]*DirHandle),

		Uid: os.Getuid(),
		Gid: os.Getgid(),
	}
}

// Path returns the path to the mount point.
func (fs *FileSystem) Path() string { return fs.path }

// Store returns the underlying store.
func (fs *FileSystem) Store() *litefs.Store { return fs.store }

// Mount mounts the file system to the mount point.
func (fs *FileSystem) Mount() (err error) {
	// Create FUSE server and mount it.
	fs.server, err = fuse.NewServer(fs, fs.path, &fuse.MountOptions{
		Name:        "litefs",
		Debug:       fs.Debug,
		EnableLocks: true,
	})
	if err != nil {
		return err
	}

	go fs.server.Serve()

	return fs.server.WaitMount()
}

// Unmount unmounts the file system.
func (fs *FileSystem) Unmount() (err error) {
	if fs.server != nil {
		if e := fs.server.Unmount(); err == nil {
			err = e
		}
	}
	return err
}

// This is called on processing the first request. The
// filesystem implementation can use the server argument to
// talk back to the kernel (through notify methods).
func (fs *FileSystem) Init(server *fuse.Server) {
}

func (fs *FileSystem) String() string { return "litefs" }

func (fs *FileSystem) SetDebug(dbg bool) {}

// InodeNotify invalidates a section of a database file in the kernel page cache.
func (fs *FileSystem) InodeNotify(dbID uint64, off int64, length int64) error {
	ino := fs.dbIno(dbID, litefs.FileTypeDatabase)
	if code := fs.server.InodeNotify(ino, off, length); code != fuse.OK {
		return errnoError(code)
	}
	return nil
}

func (fs *FileSystem) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) (code fuse.Status) {
	// Ensure lookup is only performed on top-level directory.
	if header.NodeId != rootNodeID {
		log.Printf("fuse: lookup(): invalid inode: %d", header.NodeId)
		return fuse.EINVAL
	}

	dbName, fileType := ParseFilename(name)
	db := fs.store.FindDBByName(dbName)
	if db == nil {
		return fuse.ENOENT
	}

	attr, err := fs.dbFileAttr(db, fileType)
	if os.IsNotExist(err) {
		return fuse.ENOENT
	} else if err != nil {
		log.Printf("fuse: lookup(): attr error: %s", err)
		return fuse.EIO
	}

	out.NodeId = attr.Ino
	out.Generation = 1
	out.Attr = attr
	return fuse.OK
}

func (fs *FileSystem) GetAttr(cancel <-chan struct{}, input *fuse.GetAttrIn, out *fuse.AttrOut) (code fuse.Status) {
	// Handle root directory.
	if input.NodeId == rootNodeID {
		out.Attr = fuse.Attr{
			Ino:     rootNodeID,
			Mode:    040777,
			Nlink:   1,
			Blksize: 4096,
			Owner: fuse.Owner{
				Uid: uint32(fs.Uid),
				Gid: uint32(fs.Gid),
			},
		}
		return fuse.OK
	}

	dbID, fileType, err := ParseInode(input.NodeId)
	if err != nil {
		log.Printf("fuse: getattr(): cannot parse inode: %d", input.NodeId)
		return fuse.ENOENT
	}

	db := fs.store.FindDB(dbID)
	if db == nil {
		return fuse.ENOENT
	}

	attr, err := fs.dbFileAttr(db, fileType)
	if os.IsNotExist(err) {
		return fuse.ENOENT
	} else if err != nil {
		log.Printf("fuse: getattr(): attr error: %s", err)
		return fuse.EIO
	}

	out.Attr = attr
	return fuse.OK
}

func (fs *FileSystem) Open(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) (code fuse.Status) {
	dbID, fileType, err := ParseInode(input.NodeId)
	if err != nil {
		log.Printf("fuse: open(): cannot parse inode: %d", input.NodeId)
		return fuse.ENOENT
	}

	db := fs.store.FindDB(dbID)
	if db == nil {
		return fuse.ENOENT
	}

	f, err := os.OpenFile(filepath.Join(db.Path(), FileTypeFilename(fileType)), int(input.Flags), os.FileMode(input.Mode))
	if err != nil {
		log.Printf("fuse: open(): cannot open file: %s", err)
		return toErrno(err)
	}

	fh := fs.NewFileHandle(db, fileType, f)
	out.Fh = fh.ID()
	out.OpenFlags = input.Flags

	return fuse.OK
}

func (fs *FileSystem) Unlink(cancel <-chan struct{}, input *fuse.InHeader, name string) (code fuse.Status) {
	// Ensure command is only performed on top-level directory.
	if input.NodeId != rootNodeID {
		log.Printf("fuse: unlink(): invalid parent inode: %d", input.NodeId)
		return fuse.EINVAL
	}

	dbName, fileType := ParseFilename(name)

	switch fileType {
	case litefs.FileTypeDatabase:
		return fs.unlinkDatabase(cancel, input, dbName)
	case litefs.FileTypeJournal:
		return fs.unlinkJournal(cancel, input, dbName)
	case litefs.FileTypeWAL:
		return fs.unlinkWAL(cancel, input, dbName)
	case litefs.FileTypeSHM:
		return fs.unlinkSHM(cancel, input, dbName)
	default:
		return fuse.EINVAL
	}
}

func (fs *FileSystem) unlinkDatabase(cancel <-chan struct{}, input *fuse.InHeader, dbName string) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) unlinkJournal(cancel <-chan struct{}, input *fuse.InHeader, dbName string) (code fuse.Status) {
	db := fs.store.FindDBByName(dbName)
	if db == nil {
		return fuse.ENOENT
	}

	if err := db.UnlinkJournal(); err != nil {
		log.Printf("fuse: unlink(): cannot delete journal: %s", err)
		return toErrno(err)
	}
	return fuse.OK
}

func (fs *FileSystem) unlinkWAL(cancel <-chan struct{}, input *fuse.InHeader, dbName string) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) unlinkSHM(cancel <-chan struct{}, input *fuse.InHeader, dbName string) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Create(cancel <-chan struct{}, input *fuse.CreateIn, name string, out *fuse.CreateOut) (code fuse.Status) {
	if input.NodeId != rootNodeID {
		log.Printf("fuse: lookup(): invalid inode: %d", input.NodeId)
		return fuse.EINVAL
	}

	dbName, fileType := ParseFilename(name)

	switch fileType {
	case litefs.FileTypeDatabase:
		return fs.createDatabase(cancel, input, dbName, out)
	case litefs.FileTypeJournal:
		return fs.createJournal(cancel, input, dbName, out)
	case litefs.FileTypeWAL:
		return fs.createWAL(cancel, input, dbName, out)
	case litefs.FileTypeSHM:
		return fs.createSHM(cancel, input, dbName, out)
	default:
		return fuse.EINVAL
	}
}

func (fs *FileSystem) createDatabase(cancel <-chan struct{}, input *fuse.CreateIn, dbName string, out *fuse.CreateOut) (code fuse.Status) {
	db, file, err := fs.store.CreateDB(dbName)
	if err == litefs.ErrDatabaseExists {
		return fuse.Status(syscall.EEXIST)
	} else if err != nil {
		log.Printf("fuse: create(): cannot create database: %s", err)
		return toErrno(err)
	}

	attr, err := fs.dbFileAttr(db, litefs.FileTypeDatabase)
	if err != nil {
		log.Printf("fuse: create(): cannot stat database file: %s", err)
		return toErrno(err)
	}

	ino := fs.dbIno(db.ID(), litefs.FileTypeDatabase)
	fh := fs.NewFileHandle(db, litefs.FileTypeDatabase, file)
	out.Fh = fh.ID()
	out.NodeId = ino
	out.Attr = attr

	return fuse.OK
}

func (fs *FileSystem) createJournal(cancel <-chan struct{}, input *fuse.CreateIn, dbName string, out *fuse.CreateOut) (code fuse.Status) {
	db := fs.store.FindDBByName(dbName)
	if db == nil {
		log.Printf("fuse: create(): cannot create journal, database not found: %s", dbName)
		return fuse.Status(syscall.ENOENT)
	}

	file, err := db.CreateJournal()
	if err != nil {
		log.Printf("fuse: create(): cannot find journal: %s", err)
		return toErrno(err)
	}

	attr, err := fs.dbFileAttr(db, litefs.FileTypeJournal)
	if err != nil {
		log.Printf("fuse: create(): cannot stat journal file: %s", err)
		return toErrno(err)
	}

	ino := fs.dbIno(db.ID(), litefs.FileTypeJournal)
	fh := fs.NewFileHandle(db, litefs.FileTypeJournal, file)
	out.Fh = fh.ID()
	out.NodeId = ino
	out.Attr = attr

	return fuse.OK
}

func (fs *FileSystem) createWAL(cancel <-chan struct{}, input *fuse.CreateIn, dbName string, out *fuse.CreateOut) (code fuse.Status) {
	return fuse.ENOSYS // TODO
}

func (fs *FileSystem) createSHM(cancel <-chan struct{}, input *fuse.CreateIn, dbName string, out *fuse.CreateOut) (code fuse.Status) {
	return fuse.ENOSYS // TODO
}

func (fs *FileSystem) Read(cancel <-chan struct{}, input *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	fh := fs.FileHandle(input.Fh)
	if fh == nil {
		log.Printf("fuse: read(): bad file handle: %d", input.Fh)
		return nil, fuse.EBADF
	}

	//n, err := fh.File().ReadAt(buf, int64(input.Offset))
	//if err == io.EOF {
	//	return fuse.ReadResultData(nil), fuse.OK
	//} else if err != nil {
	//	log.Printf("fuse: read(): cannot read: %s", err)
	//	return nil, fuse.EIO
	//}
	//return fuse.ReadResultData(buf[:n]), fuse.OK

	return fuse.ReadResultFd(fh.File().Fd(), int64(input.Offset), int(input.Size)), fuse.OK
}

func (fs *FileSystem) Write(cancel <-chan struct{}, input *fuse.WriteIn, data []byte) (written uint32, code fuse.Status) {
	fh := fs.FileHandle(input.Fh)
	if fh == nil {
		log.Printf("fuse: write(): invalid file handle: %d", input.Fh)
		return 0, fuse.EBADF
	}

	switch fh.FileType() {
	case litefs.FileTypeDatabase:
		return fs.writeDatabase(cancel, fh, input, data)
	case litefs.FileTypeJournal:
		return fs.writeJournal(cancel, fh, input, data)
	case litefs.FileTypeWAL:
		return fs.writeWAL(cancel, fh, input, data)
	case litefs.FileTypeSHM:
		return fs.writeSHM(cancel, fh, input, data)
	default:
		log.Printf("fuse: write(): file handle has invalid file type: %d", fh.FileType())
		return 0, fuse.EINVAL
	}
}

func (fs *FileSystem) writeDatabase(cancel <-chan struct{}, fh *FileHandle, input *fuse.WriteIn, data []byte) (written uint32, code fuse.Status) {
	if err := fh.DB().WriteDatabase(fh.File(), data, int64(input.Offset)); err != nil {
		return 0, toErrno(err)
	}
	return uint32(len(data)), fuse.OK
}

func (fs *FileSystem) writeJournal(cancel <-chan struct{}, fh *FileHandle, input *fuse.WriteIn, data []byte) (written uint32, code fuse.Status) {
	if err := fh.DB().WriteJournal(fh.File(), data, int64(input.Offset)); err != nil {
		return 0, toErrno(err)
	}
	return uint32(len(data)), fuse.OK
}

func (fs *FileSystem) writeWAL(cancel <-chan struct{}, fh *FileHandle, input *fuse.WriteIn, data []byte) (written uint32, code fuse.Status) {
	return 0, fuse.ENOSYS // TODO
}

func (fs *FileSystem) writeSHM(cancel <-chan struct{}, fh *FileHandle, input *fuse.WriteIn, data []byte) (written uint32, code fuse.Status) {
	return 0, fuse.ENOSYS // TODO
}

func (fs *FileSystem) Flush(cancel <-chan struct{}, input *fuse.FlushIn) fuse.Status {
	fh := fs.FileHandle(input.Fh)
	if fh == nil {
		log.Printf("fuse: flush(): bad file handle: %d", input.Fh)
		return fuse.EBADF
	}

	if err := fh.File().Close(); err != nil {
		log.Printf("fuse: flush(): cannot close file: %s", err)
		return toErrno(err)
	}
	return fuse.OK
}

func (fs *FileSystem) Release(cancel <-chan struct{}, input *fuse.ReleaseIn) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fh := fs.fileHandles[input.Fh]; fh != nil {
		_ = fh.Close()
		delete(fs.fileHandles, input.Fh)
	}
}

func (fs *FileSystem) Fsync(cancel <-chan struct{}, input *fuse.FsyncIn) (code fuse.Status) {
	fh := fs.FileHandle(input.Fh)
	if fh == nil {
		log.Printf("fuse: fsync(): bad file handle: %d", input.Fh)
		return fuse.EBADF
	}

	if err := fh.File().Sync(); err != nil {
		log.Printf("fuse: fsync(): cannot sync: %s", err)
		return toErrno(err)
	}
	return fuse.OK
}

func (fs *FileSystem) GetLk(cancel <-chan struct{}, in *fuse.LkIn, out *fuse.LkOut) (code fuse.Status) {
	fh := fs.FileHandle(in.Fh)
	if fh == nil {
		log.Printf("fuse: setlk(): bad file handle: %d", in.Fh)
		return fuse.EBADF
	}

	// If a lock could not be obtained, return a write lock in its place.
	// This isn't technically correct but it's good enough for SQLite usage.
	if !fh.Getlk(in.Lk.Typ, ParseLockRange(in.Lk.Start, in.Lk.End)) {
		out.Lk = fuse.FileLock{
			Start: in.Lk.Start,
			End:   in.Lk.End,
			Typ:   syscall.F_WRLCK,
		}
		return fuse.OK
	}

	// If lock could be obtained, return UNLCK.
	out.Lk = fuse.FileLock{
		Start: in.Lk.Start,
		End:   in.Lk.End,
		Typ:   syscall.F_UNLCK,
	}
	return fuse.OK
}

func (fs *FileSystem) SetLk(cancel <-chan struct{}, in *fuse.LkIn) (code fuse.Status) {
	fh := fs.FileHandle(in.Fh)
	if fh == nil {
		log.Printf("fuse: setlk(): bad file handle: %d", in.Fh)
		return fuse.EBADF
	}

	if !fh.Setlk(in.Lk.Typ, ParseLockRange(in.Lk.Start, in.Lk.End)) {
		return fuse.EAGAIN
	}
	return fuse.OK
}

func (fs *FileSystem) SetLkw(cancel <-chan struct{}, in *fuse.LkIn) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) OpenDir(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) (status fuse.Status) {
	out.Fh = fs.NewDirHandle().ID()
	out.OpenFlags = input.Flags
	return fuse.OK
}

func (fs *FileSystem) ReadDirPlus(cancel <-chan struct{}, input *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	h := fs.DirHandle(input.Fh)
	if h == nil {
		log.Printf("fuse: readdirplus(): bad file handle: %d", input.Fh)
		return fuse.EBADF
	}

	// Read & sort list of databases from the store.
	dbs := fs.store.DBs()
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Name() < dbs[j].Name() })

	// Iterate over databases starting from the offset.
	for i, db := range dbs {
		if i < h.offset {
			continue
		}

		// Write the entry to the buffer; if nil returned then buffer is full.
		if out.AddDirLookupEntry(fuse.DirEntry{
			Name: db.Name(),
			Ino:  fs.dbIno(db.ID(), litefs.FileTypeDatabase),
			Mode: 0100666},
		) == nil {
			break
		}

		h.offset++
	}
	return fuse.OK
}

func (fs *FileSystem) ReadDir(cancel <-chan struct{}, input *fuse.ReadIn, l *fuse.DirEntryList) fuse.Status {
	return fuse.ENOSYS
}

func (fs *FileSystem) ReleaseDir(input *fuse.ReleaseIn) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.dirHandles, input.Fh)
}

func (fs *FileSystem) FsyncDir(cancel <-chan struct{}, input *fuse.FsyncIn) (code fuse.Status) {
	return fuse.OK
}

func (fs *FileSystem) Fallocate(cancel <-chan struct{}, in *fuse.FallocateIn) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) CopyFileRange(cancel <-chan struct{}, input *fuse.CopyFileRangeIn) (written uint32, code fuse.Status) {
	return 0, fuse.ENOSYS
}

func (fs *FileSystem) Lseek(cancel <-chan struct{}, in *fuse.LseekIn, out *fuse.LseekOut) fuse.Status {
	return fuse.ENOSYS
}

func (fs *FileSystem) SetAttr(cancel <-chan struct{}, input *fuse.SetAttrIn, out *fuse.AttrOut) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Readlink(cancel <-chan struct{}, header *fuse.InHeader) (out []byte, code fuse.Status) {
	return nil, fuse.ENOSYS
}

func (fs *FileSystem) Mknod(cancel <-chan struct{}, input *fuse.MknodIn, name string, out *fuse.EntryOut) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Mkdir(cancel <-chan struct{}, input *fuse.MkdirIn, name string, out *fuse.EntryOut) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Rmdir(cancel <-chan struct{}, header *fuse.InHeader, name string) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Symlink(cancel <-chan struct{}, header *fuse.InHeader, pointedTo string, linkName string, out *fuse.EntryOut) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Rename(cancel <-chan struct{}, input *fuse.RenameIn, oldName string, newName string) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) Link(cancel <-chan struct{}, input *fuse.LinkIn, name string, out *fuse.EntryOut) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) GetXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string, dest []byte) (size uint32, code fuse.Status) {
	return 0, fuse.ENOSYS
}

func (fs *FileSystem) SetXAttr(cancel <-chan struct{}, input *fuse.SetXAttrIn, attr string, data []byte) fuse.Status {
	return fuse.ENOSYS
}

// ListXAttr lists extended attributes as '\0' delimited byte
// slice, and return the number of bytes. If the buffer is too
// small, return ERANGE, with the required buffer size.
func (fs *FileSystem) ListXAttr(cancel <-chan struct{}, header *fuse.InHeader, dest []byte) (n uint32, code fuse.Status) {
	return 0, fuse.ENOSYS
}

func (fs *FileSystem) RemoveXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string) fuse.Status {
	return fuse.ENOSYS
}

func (fs *FileSystem) Access(cancel <-chan struct{}, input *fuse.AccessIn) (code fuse.Status) {
	return fuse.ENOSYS
}

func (fs *FileSystem) StatFs(cancel <-chan struct{}, header *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	return fuse.ENOSYS
}

func (fs *FileSystem) Forget(nodeID, nlookup uint64) {}

// dbIno returns the inode for a given database's file.
func (fs FileSystem) dbIno(dbID uint64, fileType litefs.FileType) uint64 {
	return (uint64(dbID) << 4) | FileTypeInode(fileType)
}

// dbFileAttr returns an attribute for a given database file.
func (fs FileSystem) dbFileAttr(db *litefs.DB, fileType litefs.FileType) (fuse.Attr, error) {
	// Look up stats on the internal data file. May return "not found".
	fi, err := os.Stat(filepath.Join(db.Path(), FileTypeFilename(fileType)))
	if err != nil {
		return fuse.Attr{}, err
	}

	t := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	return fuse.Attr{
		Ino:     fs.dbIno(db.ID(), fileType),
		Size:    uint64(fi.Size()),
		Atime:   uint64(t.Unix()),
		Mtime:   uint64(t.Unix()),
		Ctime:   uint64(t.Unix()),
		Mode:    0100666,
		Nlink:   1,
		Blksize: 4096,
		Owner: fuse.Owner{
			Uid: uint32(fs.Uid),
			Gid: uint32(fs.Gid),
		},
	}, nil
}

// NewFileHandle returns a new file handle associated with a database file.
func (fs *FileSystem) NewFileHandle(db *litefs.DB, fileType litefs.FileType, file *os.File) *FileHandle {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fh := NewFileHandle(fs.nextHandleID, db, fileType, file)
	fs.nextHandleID++
	fs.fileHandles[fh.ID()] = fh

	return fh
}

// FileHandle returns a file handle by ID.
func (fs *FileSystem) FileHandle(id uint64) *FileHandle {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.fileHandles[id]
}

// NewDirHandle returns a new directory handle associated with the root directory.
func (fs *FileSystem) NewDirHandle() *DirHandle {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	h := NewDirHandle(fs.nextHandleID)
	fs.nextHandleID++
	fs.dirHandles[h.ID()] = h
	return h
}

// DirHandle returns a directory handle by ID.
func (fs *FileSystem) DirHandle(id uint64) *DirHandle {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.dirHandles[id]
}

// FileHandle represents a file system handle that points to a database file.
type FileHandle struct {
	id       uint64
	db       *litefs.DB
	fileType litefs.FileType
	file     *os.File

	// SQLite locks held
	locks struct {
		pending  uint32
		shared   uint32
		reserved uint32
	}
}

// NewFileHandle returns a new instance of FileHandle.
func NewFileHandle(id uint64, db *litefs.DB, fileType litefs.FileType, file *os.File) *FileHandle {
	fh := &FileHandle{
		id:       id,
		db:       db,
		fileType: fileType,
		file:     file,
	}
	fh.locks.pending = syscall.F_UNLCK
	fh.locks.shared = syscall.F_UNLCK
	fh.locks.reserved = syscall.F_UNLCK
	return fh
}

// ID returns the file handle identifier.
func (fh *FileHandle) ID() uint64 { return fh.id }

// DB returns the database associated with the file handle.
func (fh *FileHandle) DB() *litefs.DB { return fh.db }

// FileType return the type of database file the handle is associated with.
func (fh *FileHandle) FileType() litefs.FileType { return fh.fileType }

// File return the underlying file reference.
func (fh *FileHandle) File() *os.File { return fh.file }

// ID returns the file handle identifier.
func (fh *FileHandle) Close() (err error) {
	if fh.file != nil {
		return fh.file.Close()
	}
	return nil
}

// Getlk returns true if one or more locks could be obtained.
// This function does not actually acquire the locks.
func (fh *FileHandle) Getlk(typ uint32, lockTypes []LockType) (ok bool) {
	fh.db.WithLocksMutex(func() {
		for _, lockType := range lockTypes {
			if !fh.canSetlk(typ, lockType) {
				ok = false
				return
			}
		}
		ok = true
	})
	return ok
}

// Setlk atomically transitions all locks to a new state.
// Returns false if not all locks can be transitioned.
func (fh *FileHandle) Setlk(typ uint32, lockTypes []LockType) (ok bool) {
	fh.db.WithLocksMutex(func() {
		// Ensure all locks can transition.
		for _, lockType := range lockTypes {
			if !fh.canSetlk(typ, lockType) {
				ok = false
				return
			}
		}

		// Transition locks to new state.
		for _, lockType := range lockTypes {
			fh.setlk(typ, lockType)
		}

		ok = true
	})
	return ok
}

// canSetlk returns true if the lock transition is possible.
func (fh *FileHandle) canSetlk(toState uint32, lockType LockType) bool {
	lock, fromState := fh.lockState(lockType)

	switch toState {
	case syscall.F_RDLCK:
		switch *fromState {
		case syscall.F_UNLCK:
			return !lock.Excl
		case syscall.F_RDLCK:
			return true
		case syscall.F_WRLCK:
			return true // downgrade from write lock
		}

	case syscall.F_WRLCK:
		switch *fromState {
		case syscall.F_UNLCK:
			return !lock.Excl && lock.SharedN == 0
		case syscall.F_RDLCK:
			return lock.SharedN == 1 // upgrade from read lock
		case syscall.F_WRLCK:
			return true
		}

	case syscall.F_UNLCK:
		return true
	}

	return false
}

// setlk performs the transition of the current lock state to the new state.
// The canSetlk() function should be called before to verify first.
func (fh *FileHandle) setlk(toState uint32, lockType LockType) {
	lock, fromState := fh.lockState(lockType)

	switch toState {
	case syscall.F_RDLCK:
		switch *fromState {
		case syscall.F_UNLCK:
			lock.SharedN++
		case syscall.F_WRLCK: // downgrade from write lock
			lock.Excl = false
			lock.SharedN++
		}

	case syscall.F_WRLCK:
		switch *fromState {
		case syscall.F_UNLCK:
			// assert(lock.SharedN == 0, "no shared locks allowed when obtaining excl lock")
			lock.Excl = true
		case syscall.F_RDLCK: // upgrade from read lock
			lock.Excl, lock.SharedN = true, 0
		}

	case syscall.F_UNLCK:
		switch *fromState {
		case syscall.F_RDLCK:
			lock.SharedN--
		case syscall.F_WRLCK:
			lock.Excl = false
		}
	}

	*fromState = toState
}

// lockState returns the lock & the guard for a given lock type.
func (fh *FileHandle) lockState(lockType LockType) (*litefs.DBLock, *uint32) {
	switch lockType {
	case LockTypePending:
		return fh.db.PendingLock(), &fh.locks.pending
	case LockTypeReserved:
		return fh.db.ReservedLock(), &fh.locks.reserved
	case LockTypeShared:
		return fh.db.SharedLock(), &fh.locks.shared
	default:
		panic(fmt.Sprintf("invalid lock type: %d", lockType))
	}
}

// DirHandle represents a directory handle for the root directory.
type DirHandle struct {
	id     uint64
	offset int
}

// NewDirHandle returns a new instance of DirHandle.
func NewDirHandle(id uint64) *DirHandle {
	return &DirHandle{id: id}
}

// ID returns the file handle identifier.
func (h *DirHandle) ID() uint64 { return h.id }

// FileTypeFilename returns the base name for the internal data file.
func FileTypeFilename(t litefs.FileType) string {
	switch t {
	case litefs.FileTypeDatabase:
		return "database"
	case litefs.FileTypeJournal:
		return "journal"
	case litefs.FileTypeWAL:
		return "wal"
	case litefs.FileTypeSHM:
		return "shm"
	default:
		panic(fmt.Sprintf("FileTypeFilename(): invalid file type: %d", t))
	}
}

// FileTypeInode returns the inode offset for the file type.
func FileTypeInode(t litefs.FileType) uint64 {
	switch t {
	case litefs.FileTypeDatabase:
		return 0
	case litefs.FileTypeJournal:
		return 1
	case litefs.FileTypeWAL:
		return 2
	case litefs.FileTypeSHM:
		return 3
	default:
		panic(fmt.Sprintf("FileTypeInode(): invalid file type: %d", t))
	}
}

// FileTypeFromInode returns the file type for the given inode offset.
func FileTypeFromInode(ino uint64) (litefs.FileType, error) {
	switch ino {
	case 0:
		return litefs.FileTypeDatabase, nil
	case 1:
		return litefs.FileTypeJournal, nil
	case 2:
		return litefs.FileTypeWAL, nil
	case 3:
		return litefs.FileTypeSHM, nil
	default:
		return litefs.FileTypeNone, fmt.Errorf("invalid inode file type: %d", ino)
	}
}

// ParseFilename parses a base name into database name & file type parts.
func ParseFilename(name string) (dbName string, fileType litefs.FileType) {
	if strings.HasSuffix(name, "-journal") {
		return strings.TrimSuffix(name, "-journal"), litefs.FileTypeJournal
	} else if strings.HasSuffix(name, "-wal") {
		return strings.TrimSuffix(name, "-wal"), litefs.FileTypeWAL
	} else if strings.HasSuffix(name, "-shm") {
		return strings.TrimSuffix(name, "-shm"), litefs.FileTypeSHM
	}
	return name, litefs.FileTypeDatabase
}

// ParseInode parses an inode into its database ID & file type parts.
func ParseInode(ino uint64) (dbID uint64, fileType litefs.FileType, err error) {
	if ino < 1<<4 {
		return 0, 0, fmt.Errorf("invalid inode, out of range: %d", ino)
	}

	dbID = ino >> 4
	fileType, err = FileTypeFromInode(ino & 0xF)
	if err != nil {
		return 0, 0, err
	}
	return dbID, fileType, nil
}

type LockType int

const (
	LockTypePending  = 0x40000000
	LockTypeReserved = 0x40000001
	LockTypeShared   = 0x40000002
)

// ParseLockRange returns a list of SQLite locks that are within a range.
func ParseLockRange(start, end uint64) []LockType {
	a := make([]LockType, 0, 3)
	if start <= LockTypePending && LockTypePending <= end {
		a = append(a, LockTypePending)
	}
	if start <= LockTypeReserved && LockTypeReserved <= end {
		a = append(a, LockTypeReserved)
	}
	if start <= LockTypeShared && LockTypeShared <= end {
		a = append(a, LockTypeShared)
	}
	return a
}

// toErrno converts an error to a FUSE status code.
func toErrno(err error) fuse.Status {
	if err == nil {
		return fuse.OK
	} else if os.IsNotExist(err) {
		return fuse.ENOENT
	}
	return fuse.EPERM
}

// errnoError returns the text representation of a FUSE code.
func errnoError(errno fuse.Status) error {
	switch errno {
	case fuse.OK:
		return nil
	case fuse.EACCES:
		return errors.New("EACCES")
	case fuse.EBUSY:
		return errors.New("EBUSY")
	case fuse.EAGAIN:
		return errors.New("EAGAIN")
	case fuse.EINTR:
		return errors.New("EINTR")
	case fuse.EINVAL:
		return errors.New("EINVAL")
	case fuse.EIO:
		return errors.New("EIO")
	case fuse.ENOENT:
		return errors.New("ENOENT")
	case fuse.ENOSYS:
		return errors.New("ENOSYS")
	case fuse.ENODATA:
		return errors.New("ENODATA")
	case fuse.ENOTDIR:
		return errors.New("ENOTDIR")
	case fuse.ENOTSUP:
		return errors.New("ENOTSUP")
	case fuse.EISDIR:
		return errors.New("EISDIR")
	case fuse.EPERM:
		return errors.New("EPERM")
	case fuse.ERANGE:
		return errors.New("ERANGE")
	case fuse.EXDEV:
		return errors.New("EXDEV")
	case fuse.EBADF:
		return errors.New("EBADF")
	case fuse.ENODEV:
		return errors.New("ENODEV")
	case fuse.EROFS:
		return errors.New("EROFS")
	default:
		return errors.New("ERRNO(%d)")
	}
}

// rootNodeID is the identifier of the top-level directory.
const rootNodeID = 1
