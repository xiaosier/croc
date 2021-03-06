package croc

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	log "github.com/cihub/seelog"
	"github.com/pkg/errors"
)

func (c *Croc) processFile(src string) (err error) {
	log.Debug("processing file")
	defer func() {
		log.Debug("finished processing file")
	}()
	fd := FileMetaData{}

	// pathToFile and filename are the files that should be used internally
	var pathToFile, filename string
	// first check if it is stdin
	if src == "stdin" {
		var f *os.File
		f, err = ioutil.TempFile(".", "croc-stdin-")
		if err != nil {
			return
		}
		_, err = io.Copy(f, os.Stdin)
		if err != nil {
			return
		}
		pathToFile = "."
		filename = f.Name()
		err = f.Close()
		if err != nil {
			return
		}
		// fd.Name is what the user will see
		fd.Name = filename
		fd.DeleteAfterSending = true
	} else {
		if !exists(src) {
			err = errors.Errorf("file/folder '%s' does not exist", src)
			return
		}
		pathToFile, filename = filepath.Split(filepath.Clean(src))
		fd.Name = filename
	}

	// check wether the file is a dir
	info, err := os.Stat(path.Join(pathToFile, filename))
	if err != nil {
		log.Error(err)
		return
	}
	fd.IsDir = info.Mode().IsDir()

	// zip file
	log.Debug("zipping file")
	c.crocFile, err = zipFile(path.Join(pathToFile, filename), c.UseCompression)
	if err != nil {
		log.Error(err)
		return
	}
	log.Debug("...finished zipping")
	fd.IsCompressed = c.UseCompression
	fd.IsEncrypted = c.UseEncryption

	fd.Hash, err = hashFile(c.crocFile)
	if err != nil {
		log.Error(err)
		return err
	}
	fd.Size, err = fileSize(c.crocFile)
	if err != nil {
		err = errors.Wrap(err, "could not determine filesize")
		log.Error(err)
		return err
	}

	c.cs.Lock()
	defer c.cs.Unlock()
	c.cs.channel.fileMetaData = fd
	go showIntro(c.cs.channel.codePhrase, fd)
	return
}

func (c *Croc) getFilesReady() (err error) {
	log.Debug("getting files ready")
	defer func() {
		log.Debug("files ready")
	}()
	c.cs.Lock()
	defer c.cs.Unlock()
	c.cs.channel.notSentMetaData = true
	// send metadata

	// wait until data is ready
	for {
		if c.cs.channel.fileMetaData.Name != "" {
			break
		}
		c.cs.Unlock()
		time.Sleep(10 * time.Millisecond)
		c.cs.Lock()
	}

	// get passphrase
	var passphrase []byte
	passphrase, err = c.cs.channel.Pake.SessionKey()
	if err != nil {
		log.Error(err)
		return
	}
	if c.UseEncryption {
		// encrypt file data
		c.crocFileEncrypted = tempFileName("croc-encrypted")
		err = encryptFile(c.crocFile, c.crocFileEncrypted, passphrase)
		if err != nil {
			log.Error(err)
			return
		}
		// remove the unencrypted versoin
		if err = os.Remove(c.crocFile); err != nil {
			log.Error(err)
			return
		}
		c.cs.channel.fileMetaData.IsEncrypted = true
	} else {
		c.crocFileEncrypted = c.crocFile
	}
	// split into pieces to send
	log.Debugf("splitting %s", c.crocFileEncrypted)
	if err = splitFile(c.crocFileEncrypted, len(c.cs.channel.Ports)); err != nil {
		log.Error(err)
		return
	}
	// remove the file now since we still have pieces
	if err = os.Remove(c.crocFileEncrypted); err != nil {
		log.Error(err)
		return
	}

	// encrypt meta data
	var metaDataBytes []byte
	metaDataBytes, err = json.Marshal(c.cs.channel.fileMetaData)
	if err != nil {
		log.Error(err)
		return
	}
	c.cs.channel.EncryptedFileMetaData = encrypt(metaDataBytes, passphrase)

	c.cs.channel.Update = true
	log.Debugf("updating channel with file information")
	errWrite := c.cs.channel.ws.WriteJSON(c.cs.channel)
	if errWrite != nil {
		log.Error(errWrite)
	}
	c.cs.channel.Update = false
	go func() {
		c.cs.Lock()
		c.cs.channel.fileReady = true
		c.cs.Unlock()
	}()
	return
}

func (c *Croc) processReceivedFile() (err error) {
	// cat the file received
	c.cs.Lock()
	defer c.cs.Unlock()
	c.cs.channel.FileReceived = true
	defer func() {
		c.cs.channel.Update = true
		errWrite := c.cs.channel.ws.WriteJSON(c.cs.channel)
		if errWrite != nil {
			log.Error(errWrite)
			return
		}
		c.cs.channel.Update = false
	}()

	filesToCat := make([]string, len(c.cs.channel.Ports))
	for i := range c.cs.channel.Ports {
		filesToCat[i] = c.crocFileEncrypted + "." + strconv.Itoa(i)
		log.Debugf("going to cat file %s", filesToCat[i])
	}

	// defer os.Remove(c.crocFile)
	log.Debugf("catting file into %s", c.crocFile)
	err = catFiles(filesToCat, c.crocFileEncrypted, true)
	if err != nil {
		log.Error(err)
		return
	}

	// unencrypt
	c.crocFile = tempFileName("croc-unencrypted")
	var passphrase []byte
	passphrase, err = c.cs.channel.Pake.SessionKey()
	if err != nil {
		log.Error(err)
		return
	}
	// decrypt if was encrypted on the other side
	if c.cs.channel.fileMetaData.IsEncrypted {
		err = decryptFile(c.crocFileEncrypted, c.crocFile, passphrase)
		if err != nil {
			log.Error(err)
			return
		}
		os.Remove(c.crocFileEncrypted)
	} else {
		c.crocFile = c.crocFileEncrypted
	}

	// check hash
	log.Debug("checking hash")
	var hashString string
	hashString, err = hashFile(c.crocFile)
	if err != nil {
		log.Error(err)
		return
	}
	if hashString == c.cs.channel.fileMetaData.Hash {
		log.Debug("hashes match")
	} else {
		err = errors.Errorf("hashes do not match, %s != %s", c.cs.channel.fileMetaData.Hash, hashString)
		log.Error(err)
		return
	}

	// unzip file
	err = unzipFile(c.crocFile, ".")
	if err != nil {
		log.Error(err)
		return
	}
	os.Remove(c.crocFile)
	c.cs.channel.finishedHappy = true
	return
}
