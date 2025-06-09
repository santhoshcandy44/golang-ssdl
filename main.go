package main

import (
	"errors"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

// Custom error type
type CustomAPIError struct {
	StatusCode int    `json:"-"`
	Detail     string `json:"detail"`
}

func (e *CustomAPIError) Error() string {
	return e.Detail
}

// Constants for conversion types and quality
type SlidesConversionType string

const (
	PDF       SlidesConversionType = "PDF"
	PPTX      SlidesConversionType = "PPTX"
	ImagesZip SlidesConversionType = "IMAGES_ZIP"
)

type QualityType string

const (
	HD QualityType = "HD"
	SD QualityType = "SD"
)

func main() {
	err := godotenv.Load()

	if err != nil {
		log.Fatal("Error loading .env file")
	}
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})

	// Routes
	app.Get("/", rootHandler)
	app.Get("/convert", convertHandler)

	// Start server
	log.Fatal(app.Listen(":9002"))
}

// Custom error handler
func customErrorHandler(ctx *fiber.Ctx, err error) error {
	// Default 500 status code
	code := fiber.StatusInternalServerError
	detail := "Internal Server Error"

	// Check for custom error
	var apiErr *CustomAPIError
	if errors.As(err, &apiErr) {
		code = apiErr.StatusCode
		detail = apiErr.Detail
	}

	// Return JSON response
	return ctx.Status(code).JSON(fiber.Map{
		"success": false,
		"error":   true,
		"detail":  detail,
	})
}

// Handlers
func rootHandler(c *fiber.Ctx) error {
	// Example FTP test (commented out for security)
	/*
		ftpHost := "82.25.120.208"
		ftpUser := "u979883547.admin"
		ftpPass := "a*58cSG%x5Y4*Tn62e&zp7pT"
		ftpPort := 21

		conn, err := ftp.Dial(fmt.Sprintf("%s:%d", ftpHost, ftpPort), ftp.DialWithTimeout(10*time.Second))
		if err != nil {
			return err
		}
		defer conn.Quit()

		err = conn.Login(ftpUser, ftpPass)
		if err != nil {
			return err
		}

		files, err := conn.NameList("/")
		if err != nil {
			return err
		}

		log.Println("✅ Connected to Hostinger FTP")
		log.Println("📄 Files:", files)
	*/

	return c.JSON(fiber.Map{
		"message": "API entry",
	})
}

// Query parameters struct
type ConvertParams struct {
	URL            string               `query:"url" validate:"required"`
	ConversionType SlidesConversionType `query:"conversion_type" validate:"required,oneof=pdf pptx images_zip"`
	Quality        QualityType          `query:"quality" validate:"omitempty,oneof=hd sd"`
}

func convertHandler(c *fiber.Ctx) error {
	params := new(ConvertParams)

	// Parse query parameters
	if err := c.QueryParser(params); err != nil {
		return &CustomAPIError{
			StatusCode: fiber.StatusBadRequest,
			Detail:     "Invalid query parameters",
		}
	}

	// Validate parameters
	if strings.TrimSpace(params.URL) == "" {
		return &CustomAPIError{
			StatusCode: fiber.StatusBadRequest,
			Detail:     "Url can't be empty",
		}
	}

	if params.Quality == "" {
		params.Quality = HD // Default to HD if not specified
	}

	result, err := GetSlidesDownloadLink(params.URL, params.ConversionType, params.Quality)
	if err != nil {
		return err
	}

	return c.JSON(result)
}
