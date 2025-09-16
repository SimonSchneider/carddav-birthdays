package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/SimonSchneider/goslu/config"
	"github.com/SimonSchneider/goslu/date"
	"github.com/SimonSchneider/goslu/srvu"
)

func main() {
	if err := Run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr, os.Getenv, os.Getwd); err != nil {
		log.Fatal(err)
	}
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, getEnv func(string) string, getwd func() (string, error)) error {
	cfg, err := parseConfig(args[1:], getEnv)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}
	client := &http.Client{}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
	defer cancel()
	logger := srvu.LogToOutput(log.New(stdout, "", log.LstdFlags|log.Lshortfile))

	addressBooks, err := readAddressBooks(cfg.AddressBooksFile)
	if err != nil {
		return fmt.Errorf("failed to read address books: %w", err)
	}

	mux := http.NewServeMux()

	mux.Handle("/{addressBook}", Handler(addressBooks, client, cfg.ApiKey))

	srv := &http.Server{
		BaseContext: func(listener net.Listener) context.Context {
			return ctx
		},
		Addr:    cfg.Addr,
		Handler: srvu.With(mux, srvu.WithCompression(), srvu.WithLogger(logger)),
	}
	logger.Printf("starting carddav birthdays server, listening on %s", cfg.Addr)
	return srvu.RunServerGracefully(ctx, srv, logger)
}

type Config struct {
	Addr             string
	AddressBooksFile string
	ApiKey           string
}

func parseConfig(args []string, getEnv func(string) string) (cfg Config, err error) {
	err = config.ParseInto(&cfg, flag.NewFlagSet("", flag.ExitOnError), args, getEnv)
	return cfg, err
}

func readAddressBooks(file string) (AddressBooks, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("failed to open address books file: %w", err)
	}
	defer f.Close()

	addressBooks := make([]AddressBook, 0)
	if err := json.NewDecoder(f).Decode(&addressBooks); err != nil {
		return nil, fmt.Errorf("failed to decode address books: %w", err)
	}
	return NewAddressBooks(addressBooks...), nil
}

func Handler(addressBooks AddressBooks, client *http.Client, apiKey string) http.Handler {
	return srvu.ErrHandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		key := r.FormValue("apiKey")
		if key != apiKey {
			return fmt.Errorf("invalid api key")
		}
		name := r.PathValue("addressBook")
		book, ok := addressBooks[name]
		if !ok {
			return fmt.Errorf("address book %s not found", name)
		}
		calendar, err := getBirtdaysAndGenerateIcs(ctx, client, book)
		if err != nil {
			return fmt.Errorf("failed to get birthdays: %w", err)
		}

		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("filename=%s-birthdays.ics", name))

		_, err = w.Write([]byte(calendar))
		return err
	})
}

type AddressBooks map[string]AddressBook

func NewAddressBooks(addressBooks ...AddressBook) AddressBooks {
	books := make(AddressBooks)
	for _, book := range addressBooks {
		books[book.Name] = book
	}
	return books
}

type AddressBook struct {
	Name     string
	URL      string
	Username string
	Password string
}

func getBirtdaysAndGenerateIcs(ctx context.Context, client *http.Client, book AddressBook) (string, error) {
	birthdays, err := getBirthdays(ctx, client, book)
	if err != nil {
		return "", fmt.Errorf("failed to get birthdays: %w", err)
	}
	return generateBirthdayIcs(birthdays, date.Today()), nil
}

func getBirthdays(ctx context.Context, client *http.Client, book AddressBook) ([]Birthday, error) {
	req, err := http.NewRequestWithContext(ctx, "REPORT", book.URL, strings.NewReader(birthdayRequestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.SetBasicAuth(book.Username, book.Password)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var propfindResponse MultiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&propfindResponse); err != nil {
		return nil, fmt.Errorf("failed to parse XML response: %w", err)
	}

	var birthdays []Birthday

	for _, response := range propfindResponse.Responses {
		for _, propstat := range response.Propstat {
			vcard := propstat.Prop.AddressData
			birthday := parseBirthdayVCard(vcard)
			if birthday != nil {
				birthdays = append(birthdays, *birthday)
			}
		}
	}
	return birthdays, nil
}

const birthdayRequestBody = `<?xml version="1.0" encoding="utf-8" ?>
<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:getetag/>
    <card:address-data>
      <card:prop name="N"/>
      <card:prop name="FN"/>
      <card:prop name="BDAY"/>
    </card:address-data>
  </d:prop>
</card:addressbook-query>`

// DAV multistatus response
type MultiStatus struct {
	XMLName   xml.Name   `xml:"multistatus"`
	Responses []Response `xml:"response"`
}

type Response struct {
	Href     string     `xml:"href"`
	Propstat []Propstat `xml:"propstat"`
}

type Propstat struct {
	Prop Prop `xml:"prop"`
}

type Prop struct {
	AddressData string `xml:"address-data"`
}
