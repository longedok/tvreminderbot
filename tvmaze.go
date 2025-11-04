package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"
)

type ShowSearchResult struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Language     string  `json:"language"`
	OfficialSite string  `json:"officialSite"`
	Ended        *string `json:"ended"`
	Premiered    *string `json:"premiered"`
}

type Episode struct {
	ID       int    `json:"id"`
	Season   int    `json:"season"`
	Number   int    `json:"number"`
	Name     string `json:"name"`
	Airdate  string `json:"airdate"`
	Airtime  string `json:"airtime"`
	Airstamp string `json:"airstamp"`
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns: 10,
	},
}

func SearchShow(ctx context.Context, q string) ([]ShowSearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.tvmaze.com/search/shows?q="+
			urlQueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tvmaze search: status %d", resp.StatusCode)
	}

	var raw []struct {
		Score float64          `json:"score"`
		Show  ShowSearchResult `json:"show"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]ShowSearchResult, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Show)
	}
	return out, nil
}

func FetchEpisodes(ctx context.Context, showID int) ([]Episode, error) {
	url := fmt.Sprintf("https://api.tvmaze.com/shows/%d/episodes", showID)
	log.Printf("Fetching episodes: %s", url)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var eps []Episode
	if err := json.NewDecoder(resp.Body).Decode(&eps); err != nil {
		return nil, err
	}
	return eps, nil
}

func urlQueryEscape(s string) string {
	return (&url.URL{Path: s}).EscapedPath()
}
