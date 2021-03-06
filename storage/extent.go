// Copyright 2018 The Containerfs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juju/errors"
	"github.com/tiglabs/containerfs/util"
	"github.com/tiglabs/containerfs/util/buf"
)

const (
	ExtentOpenOpt          = os.O_CREATE | os.O_RDWR | os.O_EXCL
	ExtentOpenOptOverwrite = os.O_CREATE | os.O_RDWR
)

var (
	BrokenExtentFileErr = errors.New("broken extent file error")
)

type FileInfo struct {
	FileId  int       `json:"fileId"`
	Inode   uint64    `json:"ino"`
	Size    uint64    `json:"size"`
	Crc     uint32    `json:"crc"`
	Deleted bool      `json:"deleted"`
	ModTime time.Time `json:"modTime"`
	Source  string    `json:"src"`
}

func (ei *FileInfo) FromExtent(extent Extent) {
	if extent != nil {
		ei.FileId = int(extent.ID())
		ei.Inode = extent.Ino()
		ei.Size = uint64(extent.Size())
		ei.Crc = extent.HeaderChecksum()
		ei.Deleted = extent.IsMarkDelete()
		ei.ModTime = extent.ModTime()
	}
}

func (ei *FileInfo) String() (m string) {
	source := ei.Source
	if source == "" {
		source = "none"
	}
	return fmt.Sprintf("%v_%v_%v_%v_%v_%v", ei.FileId, ei.Inode, ei.Size, ei.Crc, ei.Deleted, source)
}

// Extent is used to manage extent block file for extent store engine.
type Extent interface {
	// ID returns the identity value (extentId) of this extent entity.
	ID() uint64

	// Ino returns this inode ID of this extent block belong to.
	Ino() uint64

	// Close this extent and release FD.
	Close() error

	// InitToFS init extent data info filesystem. If entry file exist and overwrite is true,
	// this operation will clear all data of exist entry file and initialize extent header data.
	InitToFS(ino uint64, overwrite bool) error

	// RestoreFromFS restore entity data and status from entry file stored in filesystem.
	RestoreFromFS() error

	// Write data to extent.
	Write(data []byte, offset, size int64, crc uint32) (err error)

	// Read data from extent.
	Read(data []byte, offset, size int64) (crc uint32, err error)

	// Flush synchronize data to disk immediately.
	Flush() error

	// MarkDelete mark this extent as deleted.
	MarkDelete() error

	// IsMarkDelete test this extent if has been marked as delete.
	IsMarkDelete() bool

	// Size returns length of extent data exclude header.
	Size() (size int64)

	// ModTime returns the time when this extent was last modified.
	ModTime() time.Time

	// HeaderChecksum returns crc checksum value of extent header data
	// include inode data and block crc.
	HeaderChecksum() (crc uint32)
}

// FSExtent is an implementation of Extent for local regular extent file data management.
// This extent implementation manages all header info and data body in one single entry file.
// Header of extent include inode value of this extent block and crc blocks of data blocks.
type fsExtent struct {
	file       *os.File
	filePath   string
	extentId   uint64
	lock       sync.RWMutex
	header     []byte
	modifyTime time.Time
	dataSize   int64
	closeC     chan bool
	closed     bool
}

// NewExtentInCore create and returns a new extent instance.
func NewExtentInCore(name string, extentId uint64) Extent {
	e := new(fsExtent)
	e.extentId = extentId
	e.filePath = name
	e.header = make([]byte, util.BlockHeaderSize)
	e.closeC = make(chan bool)
	return e
}

// Close this extent and release FD.
func (e *fsExtent) Close() (err error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if err = e.file.Close(); err != nil {
		return
	}
	close(e.closeC)
	e.closed = true
	return
}

// Ino returns this inode ID of this extent block belong to.
func (e *fsExtent) Ino() (ino uint64) {
	ino = binary.BigEndian.Uint64(e.header[:util.BlockHeaderInoSize])
	return
}

// ID returns the identity value (extentId) of this extent entity.
func (e *fsExtent) ID() uint64 {
	return e.extentId
}

// InitToFS init extent data info filesystem. If entry file exist and overwrite is true,
// this operation will clear all data of exist entry file and initialize extent header data.
func (e *fsExtent) InitToFS(ino uint64, overwrite bool) (err error) {

	opt := ExtentOpenOpt
	if overwrite {
		opt = ExtentOpenOptOverwrite
	}

	if e.file, err = os.OpenFile(e.filePath, opt, 0666); err != nil {
		return
	}

	defer func() {
		if err != nil {
			e.file.Close()
			os.Remove(e.filePath)
		}
	}()
	//e.tryKeepSize(int(e.file.Fd()), 0, util.ExtentFileSizeLimit)
	if err = e.file.Truncate(util.BlockHeaderSize); err != nil {
		return
	}
	binary.BigEndian.PutUint64(e.header[:8], ino)
	if _, err = e.file.WriteAt(e.header[:8], 0); err != nil {
		return
	}
	emptyCrc := crc32.ChecksumIEEE(make([]byte, util.BlockSize))
	for blockNo := 0; blockNo < util.BlockCount; blockNo++ {
		if err = e.updateBlockCrc(blockNo, emptyCrc); err != nil {
			return
		}
	}
	if err = e.file.Sync(); err != nil {
		return
	}

	var (
		fileInfo os.FileInfo
	)
	if fileInfo, err = e.file.Stat(); err != nil {
		return
	}
	e.modifyTime = fileInfo.ModTime()
	e.dataSize = 0
	// go e.pendingCollapseFile()
	return
}

// RestoreFromFS restore entity data and status from entry file stored in filesystem.
func (e *fsExtent) RestoreFromFS() (err error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if e.file, err = os.OpenFile(e.filePath, os.O_RDWR, 0666); err != nil {
		if strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			err = ErrorFileNotFound
		}
		return err
	}
	var (
		info os.FileInfo
	)
	if info, err = e.file.Stat(); err != nil {
		err = fmt.Errorf("stat file %v: %v", e.file.Name(), err)
		return
	}
	if info.Size() < util.BlockHeaderSize {
		err = BrokenExtentFileErr
		return
	}
	if _, err = e.file.ReadAt(e.header, 0); err != nil {
		err = fmt.Errorf("read file %v offset %v: %v", e.file.Name(), 0, err)
		return
	}
	e.dataSize = info.Size() - util.BlockHeaderSize
	e.modifyTime = info.ModTime()
	return
}

// MarkDelete mark this extent as deleted.
func (e *fsExtent) MarkDelete() (err error) {
	e.lock.RLock()
	defer e.lock.RUnlock()
	e.header[util.MarkDeleteIndex] = util.MarkDelete
	if _, err = e.file.WriteAt(e.header, 0); err != nil {
		return
	}
	e.modifyTime = time.Now()
	return
}

// IsMarkDelete test this extent if has been marked as delete.
func (e *fsExtent) IsMarkDelete() bool {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.header[util.MarkDeleteIndex] == util.MarkDelete
}

// Size returns length of extent data exclude header.
func (e *fsExtent) Size() (size int64) {
	e.lock.RLock()
	defer e.lock.RUnlock()
	size = e.dataSize
	return
}

// ModTime returns the time when this extent was last modified.
func (e *fsExtent) ModTime() time.Time {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.modifyTime
}

// Write data to extent.
func (e *fsExtent) Write(data []byte, offset, size int64, crc uint32) (err error) {
	if err = e.checkOffsetAndSize(offset, size); err != nil {
		return
	}
	var (
		writeSize int
	)
	e.lock.RLock()
	defer e.lock.RUnlock()

	if writeSize, err = e.file.WriteAt(data[:size], int64(offset+util.BlockHeaderSize)); err != nil {
		return
	}
	blockNo := offset / util.BlockSize
	offsetInBlock := offset % util.BlockSize
	e.dataSize = int64(math.Max(float64(e.dataSize), float64(offset+size)))
	e.modifyTime = time.Now()
	if offsetInBlock == 0 {
		return e.updateBlockCrc(int(blockNo), crc)
	}

	// Prepare read buffer for block data
	var (
		blockBuffer []byte
		poolErr     error
	)
	if blockBuffer, poolErr = buf.Buffers.Get(util.BlockSize); poolErr != nil {
		blockBuffer = make([]byte, util.BlockSize)
	}
	defer buf.Buffers.Put(blockBuffer)

	remainCheckByteCnt := offsetInBlock + int64(writeSize)
	for {
		if remainCheckByteCnt <= 0 {
			break
		}
		readN, readErr := e.file.ReadAt(blockBuffer, int64(blockNo*util.BlockSize+util.BlockHeaderSize))
		if readErr != nil && readErr != io.EOF {
			err = readErr
			return
		}
		if readN == 0 {
			break
		}
		crc = crc32.ChecksumIEEE(blockBuffer[:readN])
		if err = e.updateBlockCrc(int(blockNo), crc); err != nil {
			return
		}
		if readErr == io.EOF || readN < util.BlockSize {
			break
		}
		remainCheckByteCnt -= int64(readN)
		blockNo++
	}
	return
}

// Read data from extent.
func (e *fsExtent) Read(data []byte, offset, size int64) (crc uint32, err error) {
	if err = e.checkOffsetAndSize(offset, size); err != nil {
		return
	}
	e.lock.RLock()
	defer e.lock.RUnlock()
	var (
		readN int
	)
	if readN, err = e.file.ReadAt(data[:size], offset+util.BlockHeaderSize); err != nil {
		return
	}
	if offset%util.BlockSize == 0 && readN == util.BlockSize {
		blockNo := offset / util.BlockSize
		crc = e.getBlockCrc(int(blockNo))
		return
	}
	crc = crc32.ChecksumIEEE(data)
	return
}

func (e *fsExtent) updateBlockCrc(blockNo int, crc uint32) (err error) {
	startIdx := util.BlockHeaderCrcIndex + blockNo*util.PerBlockCrcSize
	endIdx := startIdx + util.PerBlockCrcSize
	binary.BigEndian.PutUint32(e.header[startIdx:endIdx], crc)
	if _, err = e.file.WriteAt(e.header[startIdx:endIdx], int64(startIdx)); err != nil {
		return
	}
	e.modifyTime = time.Now()
	return
}

func (e *fsExtent) getBlockCrc(blockNo int) (crc uint32) {
	startIdx := util.BlockHeaderCrcIndex + blockNo*util.PerBlockCrcSize
	endIdx := startIdx + util.PerBlockCrcSize
	crc = binary.BigEndian.Uint32(e.header[startIdx:endIdx])
	return
}

func (e *fsExtent) checkOffsetAndSize(offset, size int64) error {
	if offset+size > util.BlockSize*util.BlockCount {
		return NewParamMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}
	if offset >= util.BlockCount*util.BlockSize || size == 0 {
		return NewParamMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}

	if size > util.BlockSize {
		return NewParamMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}
	return nil
}

// Flush synchronize data to disk immediately.
func (e *fsExtent) Flush() (err error) {
	err = e.file.Sync()
	return
}

// HeaderChecksum returns crc checksum value of extent header data
// include inode data and block crc.
func (e *fsExtent) HeaderChecksum() (crc uint32) {
	e.lock.RLock()
	defer e.lock.RUnlock()
	crc = crc32.ChecksumIEEE(e.header)
	return
}

func (e *fsExtent) pendingCollapseFile() {
	timer := time.NewTimer(5 * time.Second)
	for {
		select {
		case <-timer.C:
			stat, err := e.file.Stat()
			if err != nil {
				return
			}
			if time.Now().Unix()-stat.ModTime().Unix() > 5*60 {
				e.collapseFile()
				return
			}
			continue
		case <-e.closeC:
			e.collapseFile()
			return
		}
	}
}

func (e *fsExtent) collapseFile() (err error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	var (
		info os.FileInfo
	)
	if info, err = e.file.Stat(); err != nil {
		return
	}
	statFs := &syscall.Statfs_t{}
	if err = syscall.Statfs(e.filePath, statFs); err != nil {
		return
	}
	blockNum := info.Size() / int64(statFs.Bsize)
	if info.Size()%int64(statFs.Bsize) != 0 {
		blockNum += 1
	}
	err = e.tryPunchHole(int(e.file.Fd()), blockNum*int64(statFs.Bsize), util.ExtentFileSizeLimit)
	return
}
