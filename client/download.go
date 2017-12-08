package storclient

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/JaSei/pathutil-go"
	"github.com/avast/hashutil-go"
	"github.com/avast/retry-go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type httpClient interface {
	Get(url string) (*http.Response, error)
}

//type logFieldsError interface {
//	Error() string
//	LogFields() log.Fields
//}

type downloadError struct {
	sha        hashutil.Hash
	statusCode int
	status     string
}

func (err downloadError) Error() string {
	return fmt.Sprintf("Download of %s fail %d (%s)", err.sha, err.statusCode, err.status)
}

//func (err downloadError) LogFields() log.Fields {
//	return log.Fields{
//		"sha256":     err.sha.String(),
//		"statusCode": err.statusCode,
//		"status":     err.status,
//	}
//}

func (client *StorClient) downloadWorker(id int, httpClient httpClient, shasForDownload <-chan hashutil.Hash, downloadedFilesStat chan<- DownStat) {
	defer client.wg.Done()

	log.WithField("worker", id).Debugln("Start download worker...")

	for sha := range shasForDownload {
		if sha.Equal(workerEnd) {
			log.WithField("worker", id).Debugln("worker end")
			return
		}

		filename := sha.String()
		if client.UpperCase {
			filename = strings.ToUpper(sha.String())
		}

		filename += client.Suffix

		filepath, err := pathutil.NewPath(client.downloadDir, filename)
		if err != nil {
			log.Errorf("NewPath problem: %s", err)

			downloadedFilesStat <- DownStat{Status: DOWN_FAIL}

			continue
		}

		if filepath.Exists() {
			log.WithFields(log.Fields{
				"worker": id,
				"sha256": sha.String(),
			}).Debugf("File %s exists - skip download", filepath)

			downloadedFilesStat <- DownStat{Status: DOWN_SKIP}

			continue
		}

		if !client.currentDownloads.ContainsOrAdd(sha) {
			log.WithFields(log.Fields{
				"worker": id,
				"sha256": sha.String(),
			}).Debug("File is now downloading in other worker - skip download")

			downloadedFilesStat <- DownStat{Status: DOWN_SKIP}

			continue
		}

		startTime := time.Now()

		var size int64
		err = retry.Do(
			func() error {
				var err error

				if client.Devnull {
					size, err = downloadFileToDevnull(httpClient, client.createUrl(sha), sha)
				} else {
					size, err = downloadFileViaTempFile(httpClient, filepath, client.createUrl(sha), sha)
				}

				return err
			},
			retry.OnRetry(func(n uint, err error) {
				log.WithFields(log.Fields{
					"worker": id,
					"sha256": sha.String(),
					//}).WithFields(err.(logFieldsError).LogFields()).Debugf("Retry #%d: %s", n, err)
				}).Debugf("Retry #%d: %s", n, err)
			}),
			retry.RetryIf(func(err error) bool {
				switch e := err.(type) {
				case downloadError:
					if (downloadError)(e).statusCode == 404 {
						return false
					}
				}

				return true
			}),
			retry.Delay(client.RetryDelay),
			retry.Attempts(client.RetryAttempts),
			retry.Units(1),
		)

		downloadDuration := time.Since(startTime)
		client.currentDownloads.Del(sha)

		if err != nil {
			log.WithFields(log.Fields{
				"worker": id,
				"sha256": sha.String(),
				"error":  err,
			}).Errorf("Error download %s: %s\n", sha, err)
			downloadedFilesStat <- DownStat{Status: DOWN_FAIL}
		} else {
			log.WithFields(log.Fields{
				"worker": id,
				"sha256": sha.String(),
			}).Debugf("Downloaded %s", sha)
			downloadedFilesStat <- DownStat{Size: size, Duration: downloadDuration, Status: DOWN_OK}
		}
	}
}

func (client *StorClient) newHttpClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:    client.Max,
		IdleConnTimeout: client.Timeout,
	}

	return &http.Client{Transport: tr}
}

func (client *StorClient) createUrl(sha hashutil.Hash) string {
	storage := (client.storageUrl).String()
	storage = strings.TrimRight(storage, "/")

	return fmt.Sprintf("%s/%s", storage, sha)
}

func downloadFileToDevnull(httpClient httpClient, url string, expectedSha hashutil.Hash) (size int64, err error) {
	return downloadFileToWriter(httpClient, url, ioutil.Discard, expectedSha)
}

func downloadFileViaTempFile(httpClient httpClient, filepath pathutil.Path, url string, expectedSha hashutil.Hash) (size int64, err error) {
	temppath, err := pathutil.NewPath(filepath.String() + ".temp")
	if err != nil {
		return 0, errors.Wrap(err, "Construct of new temp file fail")
	}

	// cleanup tempfile if this function fail (err is set)
	defer func() {
		if err != nil {
			if remErr := temppath.Remove(); remErr != nil {
				err = errors.Wrapf(remErr, "Cleanup tempfile %s fail", temppath)
			}
		}
	}()

	if temppath.Exists() {
		if err := temppath.Remove(); err != nil {
			return 0, errors.Wrapf(err, "Cleanup old (exists) tempfile %s fail", temppath)
		}
	}

	size, err = downloadFile(httpClient, temppath, url, expectedSha)
	if err != nil {
		return size, err
	}

	if _, err := temppath.Rename(filepath.Canonpath()); err != nil {
		return 0, errors.Wrapf(err, "Rename temp %s to final path %s fail", temppath, filepath)
	}

	return size, nil
}

func downloadFile(httpClient httpClient, path pathutil.Path, url string, expectedSha hashutil.Hash) (size int64, err error) {
	out, err := path.OpenWriter()
	if err != nil {
		return 0, errors.Wrapf(err, "OpenWriter to tempfile %s fail", path)
	}

	defer func() {
		if errClose := out.Close(); errClose != nil {
			err = errors.Wrapf(err, "Close %s fail", path)
		}
	}()

	return downloadFileToWriter(httpClient, url, out, expectedSha)
}

func downloadFileToWriter(httpClient httpClient, url string, out io.Writer, expectedSha hashutil.Hash) (size int64, err error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return 0, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			err = errClose
		}
	}()

	if resp.StatusCode != 200 {
		return 0, downloadError{sha: expectedSha, statusCode: resp.StatusCode, status: resp.Status}
	}

	hasher := sha256.New()
	multi := io.MultiWriter(out, hasher)

	size, err = io.Copy(multi, resp.Body)
	if err != nil {
		return 0, err
	}

	downSha256, err := hashutil.BytesToHash(sha256.New(), hasher.Sum(nil))
	if err != nil {
		return 0, err
	}

	if !downSha256.Equal(expectedSha) {
		return 0, fmt.Errorf("Downloaded sha (%s) is not equal with expected sha (%s)", downSha256, expectedSha)
	}

	return size, nil
}
