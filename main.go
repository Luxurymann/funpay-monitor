package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
	"github.com/fatih/color"
	"github.com/joho/godotenv"
)

// Offer represents an item you are selling
type Offer struct {
	Name        string
	Price       float64
	CategoryURL string
}

func main() {
	// 1. Load config
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, will use default/environment variables if any")
	}

	profileURL := os.Getenv("PROFILE_URL")
	if profileURL == "" {
		log.Fatal("PROFILE_URL is not set in .env")
	}

	categoryURL := os.Getenv("CATEGORY_URL")
	if categoryURL == "" {
		color.Red("CATEGORY_URL is not set in .env. Пожалуйста, укажите ссылку на категорию!")
		os.Exit(1)
	}
	color.Green("Using CATEGORY_URL from .env: %s", categoryURL)

	intervalStr := os.Getenv("CHECK_INTERVAL_SECONDS")
	intervalSec := 300
	if intervalStr != "" {
		if val, err := strconv.Atoi(intervalStr); err == nil {
			intervalSec = val
		}
	}

	color.Cyan("=== FunPay Price Monitor ===")
	color.Cyan("Monitoring profile: %s", profileURL)
	color.Cyan("Check interval: %d seconds\n", intervalSec)

	// Set up Headless Chrome options
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true), // change to false if you want to see the browser window
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	// Create a single browser instance that we will reuse
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Pre-start the browser to avoid timeout on first run
	if err := chromedp.Run(browserCtx); err != nil {
		color.Red("Failed to start headless browser: %v", err)
		return
	}

	// Extract user ID from profile URL (e.g. https://funpay.com/users/6377165/ -> 6377165)
	parts := strings.Split(strings.TrimRight(profileURL, "/"), "/")
	myUserID := parts[len(parts)-1]

	for {
		color.Yellow("\n[%s] Starting check...", time.Now().Format("15:04:05"))

		myOffers, err := fetchProfileOffers(browserCtx, profileURL, categoryURL)
		if err != nil {
			color.Red("Failed to fetch profile: %v", err)
		} else if len(myOffers) == 0 {
			color.Red("No offers found on profile. Maybe Cloudflare blocked us or HTML changed.")
		} else {
			color.Green("Found %d offers on your profile.", len(myOffers))

			// Create or open the report file
			f, errFile := os.OpenFile("report.md", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if errFile != nil {
				color.Red("Could not open report.md for writing: %v", errFile)
			} else {
				f.WriteString(fmt.Sprintf("\n## Проверка от %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
				f.WriteString("| Предмет | Моя цена | Мин. цена конкурента | Разница | Статус |\n")
				f.WriteString("|---|---|---|---|---|\n")
			}

			for _, myOffer := range myOffers {
				checkOffer(browserCtx, myOffer, myUserID, f)
				// Delay between checks to avoid triggering anti-bot
				time.Sleep(3 * time.Second)
			}
			
			if f != nil {
				f.Close()
			}
		}

		color.Cyan("Waiting %d seconds for the next check...", intervalSec)
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}
}

func fetchProfileOffers(ctx context.Context, profileURL string, defaultCategoryURL string) ([]Offer, error) {
	// Create a context with timeout for this specific page load
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var htmlContent string

	err := chromedp.Run(ctx,
		chromedp.Navigate(profileURL),
		chromedp.Sleep(2*time.Second), // Wait for JS to render Cloudflare/content
		chromedp.OuterHTML("html", &htmlContent),
	)
	if err != nil {
		return nil, err
	}

	// DEBUG: dump HTML to see the category link structure
	os.WriteFile("profile_debug.html", []byte(htmlContent), 0644)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return nil, err
	}

	var offers []Offer

	// Parsing FunPay profile items
	doc.Find(".tc-item").Each(func(i int, s *goquery.Selection) {
		categoryURL := defaultCategoryURL

		name := strings.TrimSpace(s.Find(".tc-desc-text").Text())
		if name == "" {
			name = strings.TrimSpace(s.Find(".tc-server").Text())
		}
		
		// Clean up the name by removing category trail (e.g. "blood scythe, Предметы, Трейд")
		if strings.Contains(name, ",") {
			name = strings.TrimSpace(strings.Split(name, ",")[0])
		}


		priceStr := strings.TrimSpace(s.Find(".tc-price").Text())
		priceStr = strings.ReplaceAll(priceStr, " ", "")
		priceStr = strings.ReplaceAll(priceStr, "₽", "")

		price, _ := strconv.ParseFloat(priceStr, 64)

		if name != "" && price > 0 {
			offers = append(offers, Offer{
				Name:        name,
				Price:       price,
				CategoryURL: categoryURL,
			})
		}
	})

	return offers, nil
}

func checkOffer(ctx context.Context, myOffer Offer, myUserID string, f *os.File) {
	fmt.Printf("Checking: %s (My price: %.2f ₽) -> ", myOffer.Name, myOffer.Price)

	minPrice, err := fetchCompetitorMinPrice(ctx, myOffer, myUserID)
	if err != nil {
		color.Red("Error: %v", err)
		if f != nil {
			f.WriteString(fmt.Sprintf("| %s | %.2f ₽ | - | - | ❌ Ошибка: %v |\n", myOffer.Name, myOffer.Price, err))
		}
		return
	}

	if minPrice == 0 {
		color.Yellow("No competitors found for this item.")
		if f != nil {
			f.WriteString(fmt.Sprintf("| %s | %.2f ₽ | - | - | ⚪ Нет конкурентов |\n", myOffer.Name, myOffer.Price))
		}
		return
	}

	diffPercent := ((minPrice - myOffer.Price) / myOffer.Price) * 100

	if minPrice <= myOffer.Price*0.85 {
		color.Red("ALERT! Competitor is %.1f%% cheaper! (Min price: %.2f ₽)", -diffPercent, minPrice)
		if f != nil {
			f.WriteString(fmt.Sprintf("| %s | %.2f ₽ | **%.2f ₽** | %.1f%% | 🔴 Сильно дешевле |\n", myOffer.Name, myOffer.Price, minPrice, diffPercent))
		}
	} else if minPrice >= myOffer.Price*1.15 {
		color.Green("ALERT! Competitor is %.1f%% more expensive! (Min price: %.2f ₽)", diffPercent, minPrice)
		if f != nil {
			f.WriteString(fmt.Sprintf("| %s | %.2f ₽ | %.2f ₽ | +%.1f%% | 🟢 Сильно дороже |\n", myOffer.Name, myOffer.Price, minPrice, diffPercent))
		}
	} else {
		fmt.Printf("OK (Min competitor: %.2f ₽, Diff: %.1f%%)\n", minPrice, diffPercent)
		if f != nil {
			f.WriteString(fmt.Sprintf("| %s | %.2f ₽ | %.2f ₽ | %.1f%% | 🟡 Разница до 15%% |\n", myOffer.Name, myOffer.Price, minPrice, diffPercent))
		}
	}
}

func fetchCompetitorMinPrice(ctx context.Context, myOffer Offer, myUserID string) (float64, error) {
	// Use only the first variant for the FunPay search query (e.g. "seabeast core | sb core" -> "seabeast core")
	searchQuery := strings.TrimSpace(strings.Split(myOffer.Name, "|")[0])

	searchURL := myOffer.CategoryURL
	if !strings.Contains(searchURL, "?") {
		searchURL += "?query=" + url.QueryEscape(searchQuery)
	} else {
		searchURL += "&query=" + url.QueryEscape(searchQuery)
	}

	// Create a context with timeout for this specific page load
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var htmlContent string
	err := chromedp.Run(ctx,
		chromedp.Navigate(searchURL),
		chromedp.Sleep(2*time.Second),
		chromedp.OuterHTML("html", &htmlContent),
	)
	if err != nil {
		return 0, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return 0, err
	}

	var minPrice float64 = 0

	doc.Find(".tc-item").Each(func(i int, s *goquery.Selection) {
		// Filter by name (to ignore hidden items that didn't match the query)
		competitorName := strings.TrimSpace(s.Find(".tc-desc-text").Text())
		if competitorName == "" {
			competitorName = strings.TrimSpace(s.Find(".tc-server").Text())
		}
		
		competitorName = strings.ToLower(competitorName)
		
		matchFound := false
		for _, variant := range strings.Split(myOffer.Name, "|") {
			variant = strings.TrimSpace(strings.ToLower(variant))
			if variant != "" && strings.Contains(competitorName, variant) {
				matchFound = true
				break
			}
		}

		if !matchFound {
			return // Skip this item, it doesn't match any of our variants
		}
		
		// Skip our own items using the user ID
		sellerID, _ := s.Attr("data-user")
		if sellerID == myUserID {
			return // Skip this item, it's ours!
		}
		
		priceStr, exists := s.Find(".tc-price").Attr("data-s")
		if !exists {
			priceStr = strings.TrimSpace(s.Find(".tc-price").Text())
			priceStr = strings.ReplaceAll(priceStr, " ", "")
			priceStr = strings.ReplaceAll(priceStr, "₽", "")
		}

		price, _ := strconv.ParseFloat(priceStr, 64)
		if price > 0 {
			if minPrice == 0 || price < minPrice {
				minPrice = price
			}
		}
	})

	return minPrice, nil
}
