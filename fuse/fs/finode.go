// Package fs implements an AIStore file system.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"io"

	"github.com/NVIDIA/aistore/fuse/ais"
	"github.com/jacobsa/fuse/fuseops"
)

// Ensure interface satisfaction.
var _ Inode = &FileInode{}

type FileInode struct {
	baseInode

	parent *DirectoryInode
	object *ais.Object
}

func NewFileInode(id fuseops.InodeID, attrs fuseops.InodeAttributes, parent *DirectoryInode, object *ais.Object) Inode {
	return &FileInode{
		baseInode: newBaseInode(id, attrs, object.Name),
		parent:    parent,
		object:    object,
	}
}

func (file *FileInode) Parent() Inode {
	return file.parent
}

func (file *FileInode) IsDir() bool {
	return false
}

// REQUIRES_LOCK(file)
func (file *FileInode) Size() uint64 {
	return file.attrs.Size
}

///////////
// Reading
///////////

// REQUIRES_LOCK(file)
func (file *FileInode) Load(w io.Writer, offset int64, length int64) (n int64, err error) {
	n, err = file.object.GetChunk(w, offset, length)
	if err != nil {
		return 0, err
	}
	return n, nil
}

///////////
// Writing
///////////

// REQUIRES_LOCK(file)
func (file *FileInode) Write(data []byte, size uint64) (err error) {
	return file.object.Put(data, size)
}
