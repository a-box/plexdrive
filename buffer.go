package main

import (
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"time"

	. "github.com/claudetech/loggo/default"
	"github.com/orcaman/concurrent-map"
)

var instances cmap.ConcurrentMap
var chunkPath string
var chunkSize int64
var chunkDirMaxSize int64

func init() {
	instances = cmap.New()
}

// Buffer is a buffered stream
type Buffer struct {
	numberOfInstances int
	client            *http.Client
	object            *APIObject
	tempDir           string
	preload           bool
	chunkDir          string
}

// GetBufferInstance gets a singleton instance of buffer
func GetBufferInstance(client *http.Client, object *APIObject) (*Buffer, error) {
	if !instances.Has(object.ObjectID) {
		i, err := newBuffer(client, object)
		if nil != err {
			return nil, err
		}

		instances.Set(object.ObjectID, i)
	}

	instance, ok := instances.Get(object.ObjectID)
	// if buffer allocation failed due to race conditions it will try to fetch a new one
	if !ok {
		i, err := GetBufferInstance(client, object)
		if nil != err {
			return nil, err
		}
		instance = i
	}
	instance.(*Buffer).numberOfInstances++
	return instance.(*Buffer), nil
}

// SetChunkPath sets the global chunk path
func SetChunkPath(path string) {
	chunkPath = path
}

// SetChunkSize sets the global chunk size
func SetChunkSize(size int64) {
	chunkSize = size
}

// SetChunkDirMaxSize sets the maximum size of the chunk directory
func SetChunkDirMaxSize(size int64) {
	chunkDirMaxSize = size
}

// NewBuffer creates a new buffer instance
func newBuffer(client *http.Client, object *APIObject) (*Buffer, error) {
	Log.Infof("Starting playback of %v", object.Name)
	Log.Debugf("Creating buffer for object %v", object.ObjectID)

	tempDir := filepath.Join(chunkPath, object.ObjectID)
	if err := os.MkdirAll(tempDir, 0777); nil != err {
		Log.Debugf("%v", err)
		return nil, fmt.Errorf("Could not create temp path for object %v", object.ObjectID)
	}

	if 0 == chunkSize {
		Log.Debugf("ChunkSize was 0, setting to default (5 MB)")
		chunkSize = 5 * 1024 * 1024
	}

	buffer := Buffer{
		numberOfInstances: 0,
		client:            client,
		object:            object,
		tempDir:           tempDir,
		preload:           true,
	}

	return &buffer, nil
}

// Close all handles
func (b *Buffer) Close() error {
	b.numberOfInstances--
	if 0 == b.numberOfInstances {
		Log.Infof("Stopping playback of %v", b.object.Name)
		Log.Debugf("Stop buffering for object %v", b.object.ObjectID)

		b.preload = false
		instances.Remove(b.object.ObjectID)
	}
	return nil
}

// ReadBytes on a specific location
func (b *Buffer) ReadBytes(start, size int64, isPreload bool) ([]byte, error) {
	fOffset := start % chunkSize
	offset := start - fOffset
	offsetEnd := offset + chunkSize

	Log.Debugf("Getting object %v bytes %v - %v (is preload: %v)", b.object.ObjectID, offset, offsetEnd, isPreload)

	filename := filepath.Join(b.tempDir, strconv.Itoa(int(offset)))
	if f, err := os.Open(filename); nil == err {
		defer f.Close()
		buf := make([]byte, size)
		if n, err := f.ReadAt(buf, fOffset); nil == err && n > 0 {
			Log.Debugf("Found object %v bytes %v - %v in cache", b.object.ObjectID, offset, offsetEnd)

			// update the last modified time for files that are often in use
			if err := os.Chtimes(filename, time.Now(), time.Now()); nil != err {
				Log.Warningf("Could not update last modified time for %v", filename)
			}

			return buf[:size], nil
		}
	}

	if chunkDirMaxSize > 0 {
		if err := cleanChunkDir(chunkPath); nil != err {
			Log.Debugf("%v", err)
			return nil, fmt.Errorf("Could not delete oldest chunk")
		}
	}

	Log.Debugf("Requesting object %v bytes %v - %v from API", b.object.ObjectID, offset, offsetEnd)
	req, err := http.NewRequest("GET", b.object.DownloadURL, nil)
	if nil != err {
		return nil, err
	}

	req.Header.Add("Range", fmt.Sprintf("bytes=%v-%v", offset, offsetEnd))

	Log.Tracef("Sending HTTP Request %v", req)

	res, err := b.client.Do(req)
	if nil != err {
		return nil, err
	}

	if res.StatusCode != 206 {
		return nil, fmt.Errorf("Wrong status code %v", res)
	}

	bytes, err := ioutil.ReadAll(res.Body)
	if nil != err {
		return nil, err
	}

	f, err := os.Create(filename)
	if nil != err {
		return nil, err
	}
	defer f.Close()

	_, err = f.Write(bytes)
	if nil != err {
		return nil, err
	}

	if !isPreload && b.preload && uint64(offsetEnd) < b.object.Size {
		go func() {
			b.ReadBytes(offsetEnd+1, size, true)
		}()
	}

	return bytes[fOffset:int64(math.Min(float64(fOffset+size), float64(len(bytes))))], nil
}

// cleanChunkDir checks if the chunk folder is grown to big and clears the oldest file if necessary
func cleanChunkDir(chunkPath string) error {
	chunkDirSize, err := dirSize(chunkPath)
	if nil != err {
		return err
	}

	if chunkDirSize+chunkSize > chunkDirMaxSize {
		if err := deleteOldestFile(chunkPath); nil != err {
			return err
		}
	}

	return nil
}

// deleteOldestFile deletes the oldest file in the directory
func deleteOldestFile(path string) error {
	var fpath string
	lastMod := time.Now()

	err := filepath.Walk(path, func(file string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			modTime := info.ModTime()
			if modTime.Before(lastMod) {
				lastMod = modTime
				fpath = file
			}
		}
		return err
	})

	os.Remove(fpath)

	return err
}

// dirSize gets the total directory size
func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}
