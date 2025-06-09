package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"

	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/disintegration/imaging"
	"github.com/jlaffaye/ftp"
	"github.com/jung-kurt/gofpdf"
	"github.com/manuviswam/GoPPT/ppt"
	"github.com/valyala/fasthttp"
	_ "golang.org/x/image/webp"
	"golang.org/x/sync/semaphore"
)

// ValidateURL checks if the URL is a valid SlideShare URL
func ValidateURL(urlStr string) error {
	u, err := url.Parse(urlStr)
	if err != nil {
		return &CustomAPIError{StatusCode: 400, Detail: "Invalid URL"}
	}

	if u.Host != "www.slideshare.net" {
		return &CustomAPIError{StatusCode: 400, Detail: "Invalid SlideShare URL"}
	}

	return nil
}

// FetchSlideImages fetches all slide images from a SlideShare URL
func FetchSlideImages(urlStr string) (map[string]interface{}, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(urlStr)
	req.Header.SetMethod(fasthttp.MethodGet)

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	client := &fasthttp.Client{}
	if err := client.Do(req, resp); err != nil {
		return nil, &CustomAPIError{StatusCode: 500, Detail: "Failed to fetch the presentation page"}
	}

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, &CustomAPIError{StatusCode: resp.StatusCode(), Detail: "Failed to fetch the presentation page"}
	}

	body := resp.Body()
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, &CustomAPIError{StatusCode: 500, Detail: "Failed to parse HTML"}
	}

	title := doc.Find("title").Text()

	var allSlideImages []map[int]string
	doc.Find("img[data-testid='vertical-slide-image']").Each(func(i int, s *goquery.Selection) {
		srcset, exists := s.Attr("srcset")
		if !exists {
			return
		}

		slideResolutions := make(map[int]string)
		sources := strings.Split(srcset, ",")
		for _, src := range sources {
			parts := strings.Fields(strings.TrimSpace(src))
			if len(parts) == 2 {
				urlPart := parts[0]
				res := parts[1]
				if strings.HasSuffix(res, "w") {
					resolution, err := strconv.Atoi(res[:len(res)-1])
					if err == nil {
						slideResolutions[resolution] = urlPart
					}
				}
			}
		}

		if len(slideResolutions) > 0 {
			allSlideImages = append(allSlideImages, slideResolutions)
		}
	})

	if len(allSlideImages) == 0 {
		return nil, &CustomAPIError{StatusCode: 404, Detail: "No slide images found"}
	}

	return map[string]interface{}{
		"title":  title,
		"slides": allSlideImages,
	}, nil
}
func fetchImage(ctx context.Context, client *fasthttp.Client, urlStr string) (string, error) {
	// Build fasthttp request
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(urlStr)
	req.Header.SetMethod(fasthttp.MethodGet)

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	// Perform request with timeout (since fasthttp doesn't support context natively)
	if err := client.DoTimeout(req, resp, 20*time.Second); err != nil {
		return "", fmt.Errorf("error fetching image: %w", err)
	}

	if resp.StatusCode() != fasthttp.StatusOK {
		return "", fmt.Errorf("failed to fetch image: %s (status %d)", urlStr, resp.StatusCode())
	}

	// Decode image
	imgData := resp.Body()
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		fmt.Println(err)
		return "", err
	}

	// Create temp file
	tmpFile, err := os.CreateTemp("", "slide-*.jpg")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// Convert to RGB and encode as JPEG
	rgbImg := imaging.Clone(img)
	if err := jpeg.Encode(tmpFile, rgbImg, &jpeg.Options{Quality: 90}); err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}

func fetchImagesConcurrently(urls []string, maxConcurrency int64) ([]string, error) {
	ctx := context.Background()
	sem := semaphore.NewWeighted(maxConcurrency)
	var wg sync.WaitGroup

	client := &fasthttp.Client{}
	results := make([]string, len(urls))
	errors := make([]error, len(urls))

	for i, urlStr := range urls {
		wg.Add(1)
		go func(i int, urlStr string) {
			defer wg.Done()
			if err := sem.Acquire(ctx, 1); err != nil {
				errors[i] = err
				return
			}
			defer sem.Release(1)

			filePath, err := fetchImage(ctx, client, urlStr)
			if err != nil {
				errors[i] = err
				return
			}
			results[i] = filePath
		}(i, urlStr)
	}

	wg.Wait()

	for _, err := range errors {
		if err != nil {
			for _, file := range results {
				if file != "" {
					_ = os.Remove(file)
				}
			}
			return nil, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("Failed to fetch images: %v", err)}
		}
	}

	return results, nil
}

// convertImagePathsToPDF creates a PDF from image files
func convertImagePathsToPDF(imagePaths []string, pdfPath string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")

	for _, imgPath := range imagePaths {
		// Get image dimensions
		file, err := os.Open(imgPath)
		if err != nil {
			return err
		}
		defer file.Close()

		img, _, err := image.DecodeConfig(file)
		if err != nil {
			return err
		}

		// Calculate dimensions to fit A4
		width, height := float64(img.Width), float64(img.Height)
		pageWidth, pageHeight := pdf.GetPageSize()
		ratio := math.Min(pageWidth/width, pageHeight/height)
		width *= ratio
		height *= ratio

		pdf.AddPage()
		pdf.Image(imgPath, 0, 0, width, height, false, "", 0, "")
	}

	return pdf.OutputFileAndClose(pdfPath)
}

// uploadToFTP uploads a file to an FTP server
func uploadToFTP(filePath, remotePath string) error {
	ftpHost := os.Getenv("FTP_HOST")
	ftpUser := os.Getenv("FTP_USER")
	ftpPass := os.Getenv("FTP_PASS")
	ftpPortStr := os.Getenv("FTP_PORT")
	if ftpPortStr == "" {
		ftpPortStr = "21"
	}
	ftpPort, err := strconv.Atoi(ftpPortStr)
	if err != nil {
		return err
	}

	// Connect to FTP
	fmt.Println("Connecting...")
	conn, err := ftp.Dial(fmt.Sprintf("%s:%d", ftpHost, ftpPort), ftp.DialWithTimeout(10*time.Second))
	if err != nil {
		return err
	}
	defer conn.Quit()

	// Login
	err = conn.Login(ftpUser, ftpPass)
	if err != nil {
		return err
	}

	// Create directories if needed
	dirs := strings.Split(remotePath, "/")
	remoteDir := strings.Join(dirs[:len(dirs)-1], "/")
	remoteFile := dirs[len(dirs)-1]

	err = conn.ChangeDir("/")
	if err != nil {
		return err
	}

	for _, dir := range strings.Split(remoteDir, "/") {
		if dir == "" {
			continue
		}
		err = conn.ChangeDir(dir)
		if err != nil {
			err = conn.MakeDir(dir)
			if err != nil {
				return err
			}
			err = conn.ChangeDir(dir)
			if err != nil {
				return err
			}
		}
	}

	// Upload file
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	err = conn.Stor(remoteFile, file)
	if err != nil {
		return err
	}

	return nil
}

// ConvertURLsToPDF converts image URLs to PDF and uploads to FTP
func ConvertURLsToPDF(imageURLs []string, pdfFilename string) (string, int64, error) {
	// Download images
	imagePaths, err := fetchImagesConcurrently(imageURLs, 25000)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		for _, path := range imagePaths {
			os.Remove(path)
		}
	}()

	if len(imagePaths) == 0 {
		return "", 0, &CustomAPIError{StatusCode: 500, Detail: "No images to convert to PDF"}
	}

	// Create temp PDF file
	tmpPDF, err := os.CreateTemp("", "slides-*.pdf")
	if err != nil {
		return "", 0, err
	}
	tmpPDF.Close()
	defer os.Remove(tmpPDF.Name())

	// Convert to PDF
	err = convertImagePathsToPDF(imagePaths, tmpPDF.Name())
	if err != nil {
		return "", 0, &CustomAPIError{StatusCode: 500, Detail: err.Error()}
	}

	// Prepare FTP path
	dateStr := time.Now().Format("02012006")
	ftpDir := fmt.Sprintf("SS_DL/%s", dateStr)
	ftpPath := fmt.Sprintf("%s/%s", ftpDir, pdfFilename)

	// Upload to FTP
	err = uploadToFTP(tmpPDF.Name(), ftpPath)
	if err != nil {
		return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("FTP upload failed: %v", err)}
	}

	// Get file size
	fileInfo, err := os.Stat(tmpPDF.Name())
	if err != nil {
		return "", 0, err
	}

	return ftpPath, fileInfo.Size(), nil
}

// ConvertURLsToPPTX converts image URLs to PPTX and uploads to FTP
func ConvertURLsToPPTX(imageURLs []string, pptxFilename string) (string, int64, error) {
	// Download images
	imagePaths, err := fetchImagesConcurrently(imageURLs, 10)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		for _, path := range imagePaths {
			os.Remove(path)
		}
	}()

	// Create presentation
	p := ppt.NewPPT()

	// Add slides with images
	for _, imgPath := range imagePaths {
		err := p.AddImageSlide(imgPath)
		if err != nil {
			return "", 0, fmt.Errorf("failed to add image to slide: %v", err)
		}
	}

	// Create temp PPTX file
	tmpPPTX, err := os.CreateTemp("", "slides-*.pptx")
	if err != nil {
		return "", 0, err
	}
	tmpPPTX.Close()
	defer os.Remove(tmpPPTX.Name())

	// Save presentation
	err = p.Save(tmpPPTX.Name())
	if err != nil {
		return "", 0, fmt.Errorf("failed to save PPTX: %v", err)
	}

	// Prepare FTP path
	dateStr := time.Now().Format("02012006")
	ftpDir := fmt.Sprintf("SS_DL/%s", dateStr)
	ftpPath := fmt.Sprintf("%s/%s", ftpDir, pptxFilename)

	// Upload to FTP
	err = uploadToFTP(tmpPPTX.Name(), ftpPath)
	if err != nil {
		return "", 0, fmt.Errorf("FTP upload failed: %v", err)
	}

	// Get file size
	fileInfo, err := os.Stat(tmpPPTX.Name())
	if err != nil {
		return "", 0, err
	}

	return ftpPath, fileInfo.Size(), nil
}

// ConvertURLsToZip converts image URLs to ZIP and uploads to FTP
func ConvertURLsToZip(imageURLs []string, zipFilename string) (string, int64, error) {
	// Download images
	imagePaths, err := fetchImagesConcurrently(imageURLs, 10)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		for _, path := range imagePaths {
			os.Remove(path)
		}
	}()

	// Create temp ZIP file
	tmpZip, err := os.CreateTemp("", "slides-*.zip")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmpZip.Name())

	// Create ZIP archive
	zipWriter := zip.NewWriter(tmpZip)
	for i, imgPath := range imagePaths {
		file, err := os.Open(imgPath)
		if err != nil {
			zipWriter.Close()
			return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("Failed to open image: %v", err)}
		}

		// Create zip entry
		entryName := fmt.Sprintf("image_%d.jpg", i+1)
		zipEntry, err := zipWriter.Create(entryName)
		if err != nil {
			file.Close()
			zipWriter.Close()
			return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("Failed to create zip entry: %v", err)}
		}

		// Copy file to zip
		_, err = io.Copy(zipEntry, file)
		file.Close()
		if err != nil {
			zipWriter.Close()
			return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("Failed to write to zip: %v", err)}
		}
	}

	err = zipWriter.Close()
	if err != nil {
		return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("Failed to close zip: %v", err)}
	}

	// Prepare FTP path
	dateStr := time.Now().Format("02012006")
	ftpDir := fmt.Sprintf("SS_DL/%s", dateStr)
	ftpPath := fmt.Sprintf("%s/%s", ftpDir, zipFilename)

	// Upload to FTP
	err = uploadToFTP(tmpZip.Name(), ftpPath)
	if err != nil {
		return "", 0, &CustomAPIError{StatusCode: 500, Detail: fmt.Sprintf("FTP upload failed: %v", err)}
	}

	// Get file size
	fileInfo, err := os.Stat(tmpZip.Name())
	if err != nil {
		return "", 0, err
	}

	return ftpPath, fileInfo.Size(), nil
}

// GetSlidesDownloadLink is the main function that orchestrates the conversion
func GetSlidesDownloadLink(urlStr string, conversionType SlidesConversionType, qualityType QualityType) (map[string]interface{}, error) {
	// Validate URL
	err := ValidateURL(urlStr)
	if err != nil {
		return nil, err
	}

	// Parse URL to get document short name
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, &CustomAPIError{StatusCode: 400, Detail: "Invalid URL format"}
	}

	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(pathParts) < 2 {
		return nil, &CustomAPIError{StatusCode: 400, Detail: "Invalid SlideShare URL format"}
	}
	docShort := pathParts[len(pathParts)-2]

	// Fetch slide images
	slidesData, err := FetchSlideImages(urlStr)
	if err != nil {
		return nil, err
	}

	slides, ok := slidesData["slides"].([]map[int]string)
	if !ok {
		return nil, &CustomAPIError{StatusCode: 500, Detail: "Invalid slides data format"}
	}

	title, _ := slidesData["title"].(string)

	// Select quality
	quality := 2048
	if qualityType == SD {
		quality = 638
	}

	// Get high resolution images
	var highResImages []string
	for _, slide := range slides {
		if url, exists := slide[quality]; exists {
			highResImages = append(highResImages, url)
		}
	}

	if len(highResImages) == 0 {
		return nil, &CustomAPIError{
			StatusCode: 404,
			Detail:     fmt.Sprintf("No %dpx resolution slides found", quality),
		}
	}

	thumbnail := highResImages[0]

	// Perform conversion based on type
	var path string
	var size int64
	var message string
	switch conversionType {
	case PDF:
		path, size, err = ConvertURLsToPDF(highResImages, docShort+".pdf")
		message = "PDF generated successfully."
	case PPTX:
		path, size, err = ConvertURLsToPPTX(highResImages, docShort+".pptx")
		message = "PPTX generated successfully."
	case ImagesZip:
		path, size, err = ConvertURLsToZip(highResImages, docShort+".zip")
		message = "IMAGES ZIP generated successfully."
	default:
		return nil, &CustomAPIError{StatusCode: 400, Detail: "Unsupported conversion type"}
	}

	if err != nil {
		return nil, err
	}

	fileName := filepath.Base(path)
	baseURL := os.Getenv("BASE_URL")

	return map[string]interface{}{
		"success": true,
		"message": message,
		"data": map[string]interface{}{
			"thumbnail":            thumbnail,
			"quality":              qualityType,
			"conversion_type":      conversionType,
			"slides_download_link": fmt.Sprintf("%s/%s", baseURL, path),
			"file_name":            fileName,
			"size":                 size,
			"title":                title,
		},
	}, nil
}
