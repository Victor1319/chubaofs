// Copyright 2018 The Chubao Authors.
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
	"github.com/chubaofs/cfs/util"
	"hash/crc32"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	ExtentOpenOpt  = os.O_CREATE | os.O_RDWR | os.O_EXCL
	ExtentHasClose = -1
)

type ExtentInfo struct {
	FileID     uint64    `json:"fileId"`
	Size       uint64    `json:"size"`
	Crc        uint32    `json:"crc"`
	Inode      uint64    `json:"inode"`
	IsDeleted  bool      `json:"deleted"`
	ModifyTime time.Time `json:"modTime"`
	Source     string    `json:"src"`
	isDirty    bool
}

func (ei *ExtentInfo) FromExtent(e *Extent, crc uint32) {
	if e != nil {
		ei.Size = uint64(e.dataSize)
		if !IsTinyExtent(ei.FileID) {
			ei.Crc = crc
			ei.ModifyTime = e.ModifyTime()
			ei.isDirty = crc == 0
		}
	}
}

func (ei *ExtentInfo) String() (m string) {
	source := ei.Source
	if source == "" {
		source = "none"
	}
	return fmt.Sprintf("%v_%v_%v_%v", ei.FileID, ei.Size, ei.IsDeleted, source)
}

// Extent is an implementation of Extent for local regular extent file data management.
// This extent implementation manages all header info and data body in one single entry file.
// Header of extent include inode value of this extent block and crc blocks of data blocks.
type Extent struct {
	file       *os.File
	filePath   string
	extentID   uint64
	modifyTime time.Time
	dataSize   int64
	hasClose   int32
}

// NewExtentInCore create and returns a new extent instance.
func NewExtentInCore(name string, extentID uint64) *Extent {
	e := new(Extent)
	e.extentID = extentID
	e.filePath = name

	return e
}

func (e *Extent) HasClosed() bool {
	return atomic.LoadInt32(&e.hasClose) == ExtentHasClose
}

// Close this extent and release FD.
func (e *Extent) Close() (err error) {
	if e.HasClosed() {
		return
	}
	if err = e.file.Close(); err != nil {
		return
	}
	return
}

func (e *Extent) Exist() (exsit bool) {
	_, err := os.Stat(e.filePath)
	if err != nil {
		if os.IsExist(err) {
			return true
		}
		return false
	}
	return true
}

// InitToFS init extent data info filesystem. If entry file exist and overwrite is true,
// this operation will clear all data of exist entry file and initialize extent header data.
func (e *Extent) InitToFS(ino uint64) (err error) {
	if e.file, err = os.OpenFile(e.filePath, ExtentOpenOpt, 0666); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			e.file.Close()
			os.Remove(e.filePath)
		}
	}()

	if IsTinyExtent(e.extentID) {
		e.dataSize = 0
		return
	}
	e.modifyTime = time.Now()
	e.dataSize = 0
	return
}

// RestoreFromFS restores the entity data and status from the file stored on the filesystem.
func (e *Extent) RestoreFromFS() (err error) {
	if e.file, err = os.OpenFile(e.filePath, os.O_RDWR, 0666); err != nil {
		if strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			err = ExtentNotFoundError
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
	if IsTinyExtent(e.extentID) {
		watermark := info.Size()
		if watermark%PageSize != 0 {
			watermark = watermark + (PageSize - watermark%PageSize)
		}
		e.dataSize = watermark
		return
	}
	e.dataSize = info.Size()
	e.modifyTime = info.ModTime()
	return
}

// Size returns length of the extent (not including the header).
func (e *Extent) Size() (size int64) {
	return e.dataSize
}

// ModifyTime returns the time when this extent was modified recently.
func (e *Extent) ModifyTime() time.Time {
	return e.modifyTime
}

// WriteTiny performs write on a tiny extent.
func (e *Extent) WriteTiny(data []byte, offset, size int64, crc uint32, isUpdateSize, isSync bool) (err error) {
	index := offset + size
	if index >= math.MaxUint32 {
		return ExtentIsFullError
	}

	if _, err = e.file.WriteAt(data[:size], int64(offset)); err != nil {
		return
	}
	if isSync {
		if err = e.file.Sync(); err != nil {
			return
		}
	}
	if !isUpdateSize {
		return
	}
	if index%PageSize != 0 {
		index = index + (PageSize - index%PageSize)
	}
	e.dataSize = index

	return
}

// Write writes data to an extent.
func (e *Extent) Write(data []byte, offset, size int64, crc uint32, isUpdateSize, isSync bool, crcFunc UpdateCrcFunc) (err error) {
	if IsTinyExtent(e.extentID) {
		err = e.WriteTiny(data, offset, size, crc, isUpdateSize, isSync)
		return
	}

	if err = e.checkOffsetAndSize(offset, size); err != nil {
		return
	}
	if _, err = e.file.WriteAt(data[:size], int64(offset)); err != nil {
		return
	}
	blockNo := offset / util.BlockSize
	offsetInBlock := offset % util.BlockSize
	e.dataSize = int64(math.Max(float64(e.dataSize), float64(offset+size)))
	e.modifyTime = time.Now()
	if isSync {
		if err = e.file.Sync(); err != nil {
			return
		}
	}
	if offsetInBlock == 0 && size == util.BlockSize {
		err = crcFunc(e.extentID, uint16(blockNo), crc)
		return
	} else {
		err = crcFunc(e.extentID, uint16(blockNo), 0)
	}

	return
}

// Read reads data from an extent.
func (e *Extent) Read(data []byte, offset, size int64, isRepairRead bool) (crc uint32, err error) {
	if IsTinyExtent(e.extentID) {
		return e.ReadTiny(data, offset, size, isRepairRead)
	}
	if err = e.checkOffsetAndSize(offset, size); err != nil {
		return
	}
	if _, err = e.file.ReadAt(data[:size], offset); err != nil {
		return
	}
	crc = crc32.ChecksumIEEE(data)
	return
}

// ReadTiny read data from a tiny extent.
func (e *Extent) ReadTiny(data []byte, offset, size int64, isRepairRead bool) (crc uint32, err error) {
	_, err = e.file.ReadAt(data[:size], offset)
	if isRepairRead && err == io.EOF {
		err = nil
	}
	crc = crc32.ChecksumIEEE(data[:size])

	return
}

func (e *Extent) checkOffsetAndSize(offset, size int64) error {
	if offset+size > util.BlockSize*util.BlockCount {
		return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}
	if offset >= util.BlockCount*util.BlockSize || size == 0 {
		return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}

	if size > util.BlockSize {
		return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
	}
	return nil
}

// Flush synchronizes data to the disk.
func (e *Extent) Flush() (err error) {
	err = e.file.Sync()
	return
}

func (e *Extent) autoFixDirtyCrc(crcFunc UpdateCrcFunc, scanFunc ScanBlocksFunc) (crc uint32, err error) {
	bcs, err := scanFunc(e.extentID)
	if err != nil {
		return
	}
	data := make([]byte, len(bcs)*util.PerBlockCrcSize)
	for index := 0; index < len(bcs); index++ {
		bc := bcs[index]
		if bc.crc != 0 {
			binary.BigEndian.PutUint32(data[index*util.PerBlockCrcSize:(index+1)*util.PerBlockCrcSize], bc.crc)
			continue
		}
		data := make([]byte, util.BlockSize)
		offset := int64(int64(bc.blockNo)*util.BlockSize + util.BlockHeaderSize)
		readN, err := e.file.ReadAt(data[:util.BlockSize], offset)
		if err != io.EOF {
			break
		}
		bc.crc = crc32.ChecksumIEEE(data[:readN])
		err = crcFunc(e.extentID, bc.blockNo, bc.crc)
		if err != nil {
			return 0, nil
		}
		binary.BigEndian.PutUint32(data[index*util.PerBlockCrcSize:(index+1)*util.PerBlockCrcSize], bc.crc)
	}
	crc = crc32.ChecksumIEEE(data)
	return
}

const (
	PageSize          = 4 * util.KB
	FallocFLKeepSize  = 1
	FallocFLPunchHole = 2
)

// DeleteTiny deletes a tiny extent.
func (e *Extent) DeleteTiny(offset, size int64) (err error) {
	if int(offset)%PageSize != 0 {
		return ParameterMismatchError
	}

	if int(size)%PageSize != 0 {
		size += int64(PageSize - int(size)%PageSize)
	}
	if int(size)%PageSize != 0 {
		return ParameterMismatchError
	}
	err = syscall.Fallocate(int(e.file.Fd()), FallocFLPunchHole|FallocFLKeepSize, offset, size)

	return
}
