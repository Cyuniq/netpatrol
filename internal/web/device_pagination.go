package web

import (
	"slices"
	"strconv"
)

const defaultDevicePageSize = 50

var devicePageSizes = []int{20, defaultDevicePageSize, 100, 200}

func selectedDevicePageSize(raw string) int {
	size, _ := strconv.Atoi(raw)
	if slices.Contains(devicePageSizes, size) {
		return size
	}
	return defaultDevicePageSize
}

type devicePageLink struct {
	Number int
	URL    string
}

func devicePageLinks(page, pages int, pageURL func(int) string) []devicePageLink {
	if pages <= 1 {
		return nil
	}
	start, end := 1, pages
	if pages > 7 {
		start = min(max(page-1, 2), pages-3)
		end = start + 2
		if start <= 3 {
			start = 1
		}
		if end >= pages-2 {
			end = pages
		}
	}
	var links []devicePageLink
	if start > 1 {
		links = append(links, devicePageLink{1, pageURL(1)}, devicePageLink{})
	}
	for number := start; number <= end; number++ {
		links = append(links, devicePageLink{number, pageURL(number)})
	}
	if end < pages {
		links = append(links, devicePageLink{}, devicePageLink{pages, pageURL(pages)})
	}
	return links
}
