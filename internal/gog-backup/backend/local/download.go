package local

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"github.com/bclicn/color"
	"github.com/juju/ratelimit"
	"github.com/mscharley/gog-backup/internal/gog-backup/backend"
	"github.com/mscharley/gog-backup/pkg/gog"
)

var (
	targetDir = flag.String("local-dir", os.Getenv("HOME")+"/GoG", "The target directory to download to. (backend=local)")
)

// DownloadFile is the entrypoint for the local backend. This backend downloads all the files from GoG and stores
// them in a folder structure on the local hard drive.
func DownloadFile(retries *int, downloadBucket *ratelimit.Bucket) backend.Handler {
	return func(downloads <-chan *backend.GogFile, waitGroup *sync.WaitGroup, client *gog.Client) {
		for d := range downloads {
			path := *targetDir + "/" + d.File

			for i := 1; i <= *retries; i++ {
				filename, readerTmp, err := client.DownloadFile(d.URL)
				var reader io.Reader
				if downloadBucket == nil {
					reader = ratelimit.Reader(readerTmp, downloadBucket)
				} else {
					reader = readerTmp
				}

				if err != nil {
					log.Printf("Unable to connect to GoG: %+v", err)
					continue
				}

				// Check for version information from last time.
				versionFile := path + "/." + filename + ".version"
				if d.Version != "" {
					if lastVersion, _ := ioutil.ReadFile(versionFile); string(lastVersion) == d.Version {
						log.Printf("Skipping %s as it is already up to date.\n", d.Name)
						readerTmp.Close()
						break
					}
				} else if info, _ := os.Stat(path + "/" + filename); info != nil {
					log.Printf("Skipping %s as it is backed up and isn't versioned.\n", d.Name)
					readerTmp.Close()
					break
				}

				version := ""
				if d.Version != "" {
					version = " (version: " + color.Purple(d.Version) + ")"
				}
				fmt.Printf("%s%s\n  %s -> %s\n", d.Name, version, color.LightBlue(d.URL), color.Green(path+"/"+filename))
				err = downloadFile(reader, path, filename)
				readerTmp.Close()
				if err != nil {
					log.Printf("Unable to download file: %+v", err)
					continue
				}

				if d.Version != "" {
					// Save version information for next time.
					err = ioutil.WriteFile(versionFile, []byte(d.Version), 0666)
					if err != nil {
						log.Printf("Unable to save version file: %+v", err)
						// Good enough for this run through - we'll redownload next time and retry saving the version file then.
						break
					}
				}

				// We successfully managed to download this file, skip the rest of our retries.
				break
			}
		}

		waitGroup.Done()
	}
}

func downloadFile(reader io.Reader, path string, filename string) error {
	if filename == "" {
		return fmt.Errorf("No filename available, skipping this file")
	}

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return err
	}

	tmpfile := path + "/." + filename + ".tmp"
	outfile := path + "/" + filename
	writer, err := os.OpenFile(tmpfile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer writer.Close()

	_, err = io.Copy(writer, reader)
	if err != nil {
		return err
	}

	err = os.Rename(tmpfile, outfile)
	return err
}
