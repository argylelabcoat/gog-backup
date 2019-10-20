package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bclicn/color"
	"github.com/juju/ratelimit"
	"github.com/mscharley/gog-backup/internal/gog-backup/backend"
	"github.com/mscharley/gog-backup/internal/gog-backup/backend/local"
	"github.com/mscharley/gog-backup/internal/gog-backup/backend/s3"
	"github.com/mscharley/gog-backup/pkg/gog"
	"github.com/vharitonsky/iniflags"
)

var (
	waitGroup = new(sync.WaitGroup)
)

var (
	backendOpt     = flag.String("backend", "local", "Which backend to use for processing files to backup. The default, local, uses a folder on your hard drive.")
	refreshToken   = flag.String("refresh-token", "", "A refresh token for the GoG API.")
	retries        = flag.Int("retries", 3, "How many times to retry downloading a file before giving up.")
	cleanupTimeout = flag.Int64("cleanup-timeout", 300, "How long in seconds to allow current downloads to finish.")

	gameDownloads  = flag.Int("game-downloads", 2, "How many game downloads to do concurrently.")
	extraDownloads = flag.Int("extra-downloads", 2, "How many extras to download concurrently.")
	limitDownload  = flag.Int("limit-download", 0, "Download limit in KiB/s. (default: unlimited)")
	limitUpload    = flag.Int("limit-upload", 0, "Upload limit in KiB/s (default: unlimited)")
)

func main() {
	iniflags.Parse()

	if *refreshToken == "" {
		log.Fatalln("You must provide a refresh token for GoG.com via -refresh-token.")
	}

	client := &gog.Client{
		Client:       http.DefaultClient,
		RefreshToken: *refreshToken,
	}

	var err error
	var backendHandler backend.Handler
	var downloadBucket *ratelimit.Bucket
	var uploadBucket *ratelimit.Bucket

	if *limitDownload > 0 {
		downloadBucket = ratelimit.NewBucketWithRate(float64(*limitDownload*1024), int64(*limitDownload*1024))
	}
	if *limitUpload > 0 {
		uploadBucket = ratelimit.NewBucketWithRate(float64(*limitUpload*1024), int64(*limitDownload*1024))
	}

	switch *backendOpt {
	case "local":
		backendHandler = local.DownloadFile(retries, downloadBucket)
	case "s3":
		backendHandler, err = s3.DownloadFile(retries, uploadBucket, downloadBucket)
	default:
		log.Fatalf("Unknown backend (%s): valid values are; local, s3", *backendOpt)
	}

	if err != nil {
		log.Fatalf("Error loading the backend (%s): %+v", *backendOpt, err)
	}

	finished := make(chan bool)
	gameInfo := make(chan int64)
	gameDownload := make(chan *backend.GogFile)
	extraDownload := make(chan *backend.GogFile, 10)

	go signalHandler(finished)
	go generateGames(gameInfo, finished, client)
	go fetchDetails(gameInfo, gameDownload, extraDownload, client)

	waitGroup.Add(*gameDownloads + *extraDownloads)
	for i := 0; i < *gameDownloads; i++ {
		go backendHandler(gameDownload, waitGroup, client)
	}
	for i := 0; i < *extraDownloads; i++ {
		go backendHandler(extraDownload, waitGroup, client)
	}
	waitGroup.Wait()
}

func generateGames(games chan<- int64, finished <-chan bool, client *gog.Client) {
	page := 0
	totalPages := 1
	defer close(games)
	for page < totalPages {
		page++
		if page == 1 {
			log.Printf("Fetching page %d\n", page)
		} else {
			log.Printf("Fetching page %d/%d\n", page, totalPages)
		}
		result, err := client.GetFilteredProducts(gog.GameMediaType, page)
		if err != nil {
			log.Printf("error: %+v", err)
			return
		}

		totalPages = result.TotalPages
		for _, product := range result.Products {
			select {
			case games <- product.ID:
				// Do nothing, keep looping.
			case _ = <-finished:
				return
			}
		}
	}
}

func safePath(path string) string {
	return strings.Replace(
		strings.Replace(strings.TrimSpace(path), "/", "", -1),
		":", " -", -1)
}

func fetchDetails(games <-chan int64, gameDownload chan<- *backend.GogFile, extraDownload chan<- *backend.GogFile, client *gog.Client) {
	for id := range games {
		log.Printf("Fetching details for %d", id)
		result, err := client.GameDetails(id)
		if err != nil {
			log.Printf("Unable for fetch details for %d: %+v", id, err)
		} else {
			var games []struct {
				Path    string
				Details *gog.GameDetails
			}
			games = append(games, struct {
				Path    string
				Details *gog.GameDetails
			}{"/" + safePath(result.Title), result})
			for i := 0; i < len(games); i++ {
				path := games[i].Path[1:]
				game := games[i].Details

				for _, extra := range game.Extras {
					extraDownload <- &backend.GogFile{
						Name:    fmt.Sprintf("%s %s", color.LightPurple("Extra for "+game.Title+": "+extra.Name), color.LightYellow("["+extra.Size+"]")),
						URL:     gog.EmbedEndpoint + extra.ManualDownloadURL,
						File:    path + "/Extras",
						Version: extra.Version,
					}
				}

				if len(game.Downloads) > 0 {
					download := game.Downloads[0]
					for _, d := range download.Platforms.Windows {
						gameDownload <- &backend.GogFile{
							Name:    fmt.Sprintf("%s %s %s", color.LightPurple(d.Name), color.Red("[Windows]"), color.LightYellow("["+d.Size+"]")),
							URL:     gog.EmbedEndpoint + d.ManualDownloadURL,
							File:    path + "/Windows",
							Version: d.Version,
						}
					}
					for _, d := range download.Platforms.Mac {
						gameDownload <- &backend.GogFile{
							Name:    fmt.Sprintf("%s %s %s", color.LightPurple(d.Name), color.Red("[Mac]"), color.LightYellow("["+d.Size+"]")),
							URL:     gog.EmbedEndpoint + d.ManualDownloadURL,
							File:    path + "/Mac",
							Version: d.Version,
						}
					}
					for _, d := range download.Platforms.Linux {
						gameDownload <- &backend.GogFile{
							Name:    fmt.Sprintf("%s %s %s", color.LightPurple(d.Name), color.Red("[Linux]"), color.LightYellow("["+d.Size+"]")),
							URL:     gog.EmbedEndpoint + d.ManualDownloadURL,
							File:    path + "/Linux",
							Version: d.Version,
						}
					}
				}

				for _, dlc := range game.DLCs {
					games = append(games, struct {
						Path    string
						Details *gog.GameDetails
					}{path + "/" + safePath(dlc.Title), dlc})
				}
			}
		}
	}

	close(gameDownload)
	close(extraDownload)
}

func signalHandler(finished chan<- bool) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	signal := <-c
	finished <- true
	close(finished)
	log.Printf("Received a %s signal, finishing downloads before closing.", signal)
	timeout := time.After(time.Second * time.Duration(*cleanupTimeout))
	select {
	case signal = <-c:
		log.Fatalf("Received a second %s signal, closing down without cleanup.", signal)
	case _ = <-timeout:
		log.Fatalf("Closing after waiting %d seconds.", *cleanupTimeout)
	}
}
