/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// SHA1 is the currently accepted hash algorithm for SPDX documents, used for
// file integrity checks, NOT security.
// Instances of G401 and G505 can be safely ignored in this file.
//
// ref: https://github.com/spdx/spdx-spec/issues/11
//
//nolint:gosec
package license

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/release-utils/http"
	"sigs.k8s.io/release-utils/util"
)

// ListURL is the json list of all spdx licenses
const (
	LicenseDataURL      = "https://spdx.org/licenses/"
	LicenseListFilename = "licenses.json"
	BaseReleaseURL      = "https://github.com/spdx/license-list-data/archive/refs/tags/"
	LatestReleaseURL    = "https://api.github.com/repos/spdx/license-list-data/releases/latest"
)

// NewDownloader returns a downloader with the default options
func NewDownloader() (*Downloader, error) {
	return NewDownloaderWithOptions(DefaultDownloaderOpts)
}

// NewDownloaderWithOptions returns a downloader with specific options
func NewDownloaderWithOptions(opts *DownloaderOptions) (*Downloader, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("validating downloader options: %w", err)
	}
	impl := DefaultDownloaderImpl{}
	impl.SetOptions(opts)

	d := &Downloader{}
	d.SetImplementation(&impl)

	return d, nil
}

// DownloaderOptions is a set of options for the license downloader
type DownloaderOptions struct {
	EnableCache       bool   // Should we use the cache or not
	CacheDir          string // Directory where data will be cached, defaults to temporary dir
	parallelDownloads int    // Number of license downloads we'll do at once
}

// Validate Checks the downloader options
func (do *DownloaderOptions) Validate() error {
	// If we are using a cache
	if do.EnableCache {
		// and no cache dir was specified
		if do.CacheDir == "" {
			// use a temporary dir
			dir, err := os.MkdirTemp(os.TempDir(), "license-cache-")
			do.CacheDir = dir
			if err != nil {
				return fmt.Errorf("creating temporary directory: %w", err)
			}
		} else if !util.Exists(do.CacheDir) {
			if err := os.MkdirAll(do.CacheDir, os.FileMode(0o755)); err != nil {
				return fmt.Errorf("creating license downloader cache: %w", err)
			}
		}

		// Is we have a cache dir, check if it exists
		if !util.Exists(do.CacheDir) {
			return errors.New("the specified cache directory does not exist: " + do.CacheDir)
		}
	}
	return nil
}

// Downloader handles downloading f license data
type Downloader struct {
	impl DownloaderImplementation
}

// SetImplementation sets the implementation that will drive the downloader
func (d *Downloader) SetImplementation(di DownloaderImplementation) {
	d.impl = di
}

// GetLicenses is the mina function of the downloader. Returns a license list
// or an error if could get them
func (d *Downloader) GetLicenses() (*List, error) {
	return d.impl.GetLicenses()
}

//counterfeiter:generate . DownloaderImplementation

// DownloaderImplementation has only one method
type DownloaderImplementation interface {
	GetLicenses() (*List, error)
	SetOptions(*DownloaderOptions)
	getLatestTag() (string, error)
}

// DefaultDownloaderOpts set of options for the license downloader
var DefaultDownloaderOpts = &DownloaderOptions{
	EnableCache:       true,
	CacheDir:          "",
	parallelDownloads: 5,
}

// DefaultDownloaderImpl is the default implementation that gets licenses
type DefaultDownloaderImpl struct {
	Options *DownloaderOptions
}

// SetOptions sets the implementation options
func (ddi *DefaultDownloaderImpl) SetOptions(opts *DownloaderOptions) {
	ddi.Options = opts
}

func (ddi *DefaultDownloaderImpl) getLatestTag() (string, error) {
	data, err := http.NewAgent().Get(LatestReleaseURL)
	if err != nil {
		return "", err
	}
	type GHReleaseResp struct {
		TagName string `json:"tag_name"`
	}
	resp := GHReleaseResp{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp.TagName, nil
}

// GetLicenses downloads the main json file listing all SPDX supported licenses
func (ddi *DefaultDownloaderImpl) GetLicenses() (licenses *List, err error) {
	// TODO: Cache licenselist
	logrus.Debugf("Downloading main SPDX license data from " + LicenseDataURL)
	tag, err := ddi.getLatestTag()
	if err != nil {
		return nil, err
	}
	link := BaseReleaseURL + tag + ".zip"

	var zipData []byte
	if ddi.Options.EnableCache {
		zipData, err = ddi.getCachedData(link)
		if err != nil {
			return nil, fmt.Errorf("getting cached data: %w", err)
		}
	}

	// No cached data available
	if zipData == nil {
		zipData, err = http.NewAgent().WithTimeout(time.Hour).Get(link)
		if err != nil {
			return nil, fmt.Errorf("downloading license tarball: %w", err)
		}
		ddi.cacheData(link, zipData)
	}

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}
	licensesFile, err := reader.Open(fmt.Sprintf("license-list-data-%s/json/licenses.json", tag[1:]))
	if err != nil {
		return nil, err
	}
	licensesJSON, err := io.ReadAll(licensesFile)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(licensesJSON, &licenses); err != nil {
		return nil, fmt.Errorf("parsing SPDX licence list: %w", err)
	}
	err = fs.WalkDir(reader, fmt.Sprintf("license-list-data-%s/json/details", tag[1:]), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		licenseFile, err := reader.Open(path)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(licenseFile)
		if err != nil {
			return err
		}
		license, err := ParseLicense(data)
		if err != nil {
			return err
		}
		licenses.Add(license)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return licenses, nil
}

// cacheFileName return the cache filename for an URL
func (ddi *DefaultDownloaderImpl) cacheFileName(url string) string {
	return filepath.Join(
		ddi.Options.CacheDir, fmt.Sprintf("%x.json", sha1.Sum([]byte(url))),
	)
}

// cacheData writes data to a cache file
func (ddi *DefaultDownloaderImpl) cacheData(url string, data []byte) error {
	cacheFileName := ddi.cacheFileName(url)
	_, err := os.Stat(filepath.Dir(cacheFileName))
	if err != nil && os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(cacheFileName), os.FileMode(0o755)); err != nil {
			return fmt.Errorf("creating cache directory: %w", err)
		}
	}
	if err = os.WriteFile(cacheFileName, data, os.FileMode(0o644)); err != nil {
		return fmt.Errorf("writing cache file: %w", err)
	}
	return nil
}

// getCachedData returns cached data for an URL if we have it
func (ddi *DefaultDownloaderImpl) getCachedData(url string) ([]byte, error) {
	cacheFileName := ddi.cacheFileName(url)
	finfo, err := os.Stat(cacheFileName)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking if cached data exists: %w", err)
	}

	if err != nil {
		logrus.Debugf("No cached data for %s", url)
		return nil, nil
	}

	if finfo.Size() == 0 {
		logrus.Warn("Cached file is empty, removing")
		return nil, fmt.Errorf("removing corrupt cached file: %w", os.Remove(cacheFileName))
	}
	cachedData, err := os.ReadFile(cacheFileName)
	if err != nil {
		return nil, fmt.Errorf("reading cached data file: %w", err)
	}
	logrus.Debug("Reusing cached data from %s", url)
	return cachedData, nil
}
