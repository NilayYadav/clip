package clipfs

import (
	"context"
	"fmt"
	"log"
	"path"
	"syscall"

	"github.com/beam-cloud/clip/pkg/common"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type FSNode struct {
	fs.Inode
	filesystem   *ClipFileSystem
	clipNode     *common.ClipNode
	attr         fuse.Attr
	supportsMmap bool
}

func (n *FSNode) log(format string, v ...interface{}) {
	if n.filesystem.verbose {
		log.Printf(fmt.Sprintf("[CLIPFS] (%s) %s", n.clipNode.Path, format), v...)
	}
}

func (n *FSNode) OnAdd(ctx context.Context) {
	n.log("OnAdd called")
}

func (n *FSNode) Init(ctx context.Context) {
	n.supportsMmap = true
}

func (n *FSNode) Mmap(ctx context.Context, f fs.FileHandle, off int64, sz int64, flags uint32) (bool, error) {
	n.log("Mmap called with offset: %d, size: %d, flags: %d", off, sz, flags)

	// Check if we need to pad with null terminator
	if off+sz > int64(n.clipNode.DataLen) {
		// Create a padded buffer that includes null terminator
		paddedSize := off + sz
		paddedBuf := make([]byte, paddedSize)

		// Read the actual file content
		_, err := n.filesystem.s.ReadFile(n.clipNode, paddedBuf[:n.clipNode.DataLen], 0)
		if err != nil {
			return false, err
		}

		// Add null terminator
		paddedBuf[n.clipNode.DataLen] = 0

		// Return the padded buffer
		return true, nil
	}

	return false, nil
}

func (n *FSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.log("Getattr called")

	node := n.clipNode

	// Fill in the AttrOut struct
	out.Ino = node.Attr.Ino
	out.Size = node.Attr.Size
	out.Blocks = node.Attr.Blocks
	out.Atime = node.Attr.Atime
	out.Mtime = node.Attr.Mtime
	out.Ctime = node.Attr.Ctime
	out.Mode = node.Attr.Mode
	out.Nlink = node.Attr.Nlink
	out.Owner = node.Attr.Owner

	return fs.OK
}

func (n *FSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.log("Lookup called with name: %s", name)

	// Create the full path of the child node
	childPath := path.Join(n.clipNode.Path, name)

	// Check the cache
	n.filesystem.cacheMutex.RLock()
	entry, found := n.filesystem.lookupCache[childPath]
	n.filesystem.cacheMutex.RUnlock()
	if found {
		n.log("Lookup cache hit for name: %s", childPath)
		out.Attr = entry.attr
		return entry.inode, fs.OK
	}

	// Lookup the child node
	child := n.filesystem.s.Metadata().Get(childPath)
	if child == nil {
		// No child with the requested name exists
		return nil, syscall.ENOENT
	}

	// Fill out the child node's attributes
	out.Attr = child.Attr

	// Create a new Inode for the child
	childInode := n.NewInode(ctx, &FSNode{filesystem: n.filesystem, clipNode: child, attr: child.Attr}, fs.StableAttr{Mode: child.Attr.Mode, Ino: child.Attr.Ino})

	// Cache the result
	n.filesystem.cacheMutex.Lock()
	n.filesystem.lookupCache[childPath] = &lookupCacheEntry{inode: childInode, attr: child.Attr}
	n.filesystem.cacheMutex.Unlock()

	return childInode, fs.OK
}

func (n *FSNode) Opendir(ctx context.Context) syscall.Errno {
	n.log("Opendir called")
	return 0
}

func (n *FSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	n.log("Open called with flags: %v", flags)
	if flags&syscall.MAP_PRIVATE != 0 || flags&syscall.MAP_SHARED != 0 {
		// Set FUSE direct IO flag for mmap
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}

	return nil, fuseFlags, fs.OK
}

func (n *FSNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n.log("Read called with offset: %v", off)

	if off >= int64(n.clipNode.DataLen) {
		// Return zeros for reads beyond file size
		for i := range dest {
			dest[i] = 0
		}
		return fuse.ReadResultData(dest), fs.OK
	}

	// Length of the content to read
	length := int64(len(dest))

	// Don't even try to read 0 byte files
	if n.clipNode.DataLen == 0 {
		nRead := 0
		return fuse.ReadResultData(dest[:nRead]), fs.OK
	}

	// If we have provided a contentCache, try and use it
	// Switch back local filesystem if all content is cached on disk
	if n.filesystem.contentCacheAvailable && n.clipNode.ContentHash != "" && !n.filesystem.s.CachedLocally() {
		content, err := n.filesystem.contentCache.GetContent(n.clipNode.ContentHash, off, length)

		// Content found in cache
		if err == nil {
			copy(dest, content)
			return fuse.ReadResultData(dest[:len(content)]), fs.OK
		} else { // Cache miss - read from the underlying source and store in cache
			nRead, err := n.filesystem.s.ReadFile(n.clipNode, dest, off)
			if err != nil {
				return nil, syscall.EIO
			}

			// Store entire file in CAS
			go func() {
				n.filesystem.CacheFile(n)
			}()

			return fuse.ReadResultData(dest[:nRead]), fs.OK
		}
	}

	nRead, err := n.filesystem.s.ReadFile(n.clipNode, dest, off)
	if err != nil {
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:nRead]), fs.OK
}

func (n *FSNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	n.log("Readlink called")

	if n.clipNode.NodeType != common.SymLinkNode {
		// This node is not a symlink
		return nil, syscall.EINVAL
	}

	// Use the symlink target path directly
	symlinkTarget := n.clipNode.Target

	// In this case, we don't need to read the file
	return []byte(symlinkTarget), fs.OK
}

func (n *FSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	n.log("Readdir called")

	dirEntries := n.filesystem.s.Metadata().ListDirectory(n.clipNode.Path)
	return fs.NewListDirStream(dirEntries), fs.OK
}

func (n *FSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	n.log("Create called with name: %s, flags: %v, mode: %v", name, flags, mode)
	return nil, nil, 0, syscall.EROFS
}

func (n *FSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.log("Mkdir called with name: %s, mode: %v", name, mode)
	return nil, syscall.EROFS
}

func (n *FSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.log("Rmdir called with name: %s", name)
	return syscall.EROFS
}

func (n *FSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.log("Unlink called with name: %s", name)
	return syscall.EROFS
}

func (n *FSNode) Rename(ctx context.Context, oldName string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.log("Rename called with oldName: %s, newName: %s, flags: %v", oldName, newName, flags)
	return syscall.EROFS
}
