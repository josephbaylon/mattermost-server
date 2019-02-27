// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package config

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils/fileutils"
)

var (
	ErrReadOnlyConfiguration = errors.New("configuration is read-only")
)

// FileStore is a config store backed by a file such as config/config.json.
type FileStore struct {
	commonStore

	path    string
	watch   bool
	watcher *watcher
}

// NewFileStore creates a new instance of a config store backed by the given file path.
//
// If watch is true, any external changes to the file will force a reload.
func NewFileStore(path string, watch bool) (fs *FileStore, err error) {
	resolvedPath, err := resolveConfigFilePath(path)
	if err != nil {
		return nil, err
	}

	fs = &FileStore{
		path:  resolvedPath,
		watch: watch,
	}
	if err = fs.Load(); err != nil {
		return nil, errors.Wrap(err, "failed to load")
	}

	if fs.watch {
		if err = fs.startWatcher(); err != nil {
			mlog.Error("failed to start config watcher", mlog.String("path", path), mlog.Err(err))
		}
	}

	return fs, nil
}

// resolveConfigFilePath attempts to resolve the given configuration file path to an absolute path.
//
// Consideration is given to maintaining backwards compatibility when resolving the path to the
// configuration file.
func resolveConfigFilePath(path string) (string, error) {
	// Absolute paths are explicit and require no resolution.
	if filepath.IsAbs(path) {
		return path, nil
	}

	// Search for the given relative path (or plain filename) in various directories,
	// resolving to the corresponding absolute path if found. FindConfigFile takes into account
	// various common search paths rooted both at the current working directory and relative
	// to the executable.
	if configFile := fileutils.FindConfigFile(path); configFile != "" {
		return configFile, nil
	}

	// Otherwise, search for the config/ folder using the same heuristics as above, and build
	// an absolute path anchored there and joining the given input path (or plain filename).
	if configFolder, found := fileutils.FindDir("config"); found {
		return filepath.Join(configFolder, path), nil
	}

	// Fail altogether if we can't even find the config/ folder. This should only happen if
	// the executable is relocated away from the supporting files.
	return "", fmt.Errorf("failed to find config file %s", path)
}

// Set replaces the current configuration in its entirety, without updating the backing store.
func (fs *FileStore) Set(newCfg *model.Config) (*model.Config, error) {
	return fs.commonStore.set(newCfg, func(cfg *model.Config) error {
		if *fs.config.ClusterSettings.Enable && *fs.config.ClusterSettings.ReadOnlyConfig {
			return ErrReadOnlyConfiguration
		}

		return nil
	})
}

// persist writes the configuration to the configured file.
func (fs *FileStore) persist(cfg *model.Config) error {
	fs.stopWatcher()

	b, err := marshalConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to serialize")
	}

	err = ioutil.WriteFile(fs.path, b, 0644)
	if err != nil {
		return errors.Wrap(err, "failed to write file")
	}

	if fs.watch {
		if err = fs.startWatcher(); err != nil {
			mlog.Error("failed to start config watcher", mlog.String("path", fs.path), mlog.Err(err))
		}
	}

	return nil
}

// Load updates the current configuration from the backing store.
func (fs *FileStore) Load() (err error) {
	var needsSave bool
	var f io.ReadCloser

	f, err = os.Open(fs.path)
	if os.IsNotExist(err) {
		needsSave = true
		defaultCfg := model.Config{}
		defaultCfg.SetDefaults()

		var defaultCfgBytes []byte
		defaultCfgBytes, err = marshalConfig(&defaultCfg)
		if err != nil {
			return errors.Wrap(err, "failed to serialize default config")
		}

		f = ioutil.NopCloser(bytes.NewReader(defaultCfgBytes))

	} else if err != nil {
		return errors.Wrapf(err, "failed to open %s for reading", fs.path)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = errors.Wrap(closeErr, "failed to close")
		}
	}()

	return fs.commonStore.load(f, needsSave, fs.persist)
}

// Save writes the current configuration to the backing store.
func (fs *FileStore) Save() error {
	fs.configLock.Lock()
	defer fs.configLock.Unlock()

	return fs.persist(fs.config)
}

// startWatcher starts a watcher to monitor for external config file changes.
func (fs *FileStore) startWatcher() error {
	if fs.watcher != nil {
		return nil
	}

	watcher, err := newWatcher(fs.path, func() {
		if err := fs.Load(); err != nil {
			mlog.Error("failed to reload file on change", mlog.String("path", fs.path), mlog.Err(err))
		}
	})
	if err != nil {
		return err
	}

	fs.watcher = watcher

	return nil
}

// stopWatcher stops any previously started watcher.
func (fs *FileStore) stopWatcher() {
	if fs.watcher == nil {
		return
	}

	if err := fs.watcher.Close(); err != nil {
		mlog.Error("failed to close watcher", mlog.Err(err))
	}
	fs.watcher = nil
}

// String returns the path to the file backing the config.
func (fs *FileStore) String() string {
	return "file://" + fs.path
}

// Close cleans up resources associated with the store.
func (fs *FileStore) Close() error {
	fs.configLock.Lock()
	defer fs.configLock.Unlock()

	fs.stopWatcher()

	return nil
}