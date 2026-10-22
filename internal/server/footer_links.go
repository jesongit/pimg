package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

type FooterLink struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Icon    string `json:"icon"`
	URL     string `json:"url"`
	IconURL string `json:"icon_url,omitempty"`
}

func defaultFooterLinks() []FooterLink {
	return []FooterLink{{ID: "github", Label: "GitHub", Icon: "github", URL: "https://github.com"}}
}

var footerLinkID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validFooterURL(value string, image bool) bool {
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\\") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return true
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return false
	}
	if (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" {
		return true
	}
	return !image && u.Scheme == "mailto" && u.Opaque != ""
}

func parseFooterLinks(value any) ([]FooterLink, error) {
	items, ok := value.([]any)
	if !ok || len(items) > 12 {
		return nil, fmt.Errorf("页脚链接须为列表，最多 12 项")
	}
	encoded, err := json.Marshal(items)
	if err != nil || len(encoded) > 16384 {
		return nil, fmt.Errorf("页脚链接内容过长")
	}
	links := make([]FooterLink, 0, len(items))
	if err := json.Unmarshal(encoded, &links); err != nil {
		return nil, fmt.Errorf("页脚链接格式不正确")
	}
	seen := map[string]bool{}
	for i := range links {
		item := &links[i]
		item.Label, item.URL, item.IconURL = strings.TrimSpace(item.Label), strings.TrimSpace(item.URL), strings.TrimSpace(item.IconURL)
		if !footerLinkID.MatchString(item.ID) || seen[item.ID] {
			return nil, fmt.Errorf("页脚链接标识无效或重复")
		}
		seen[item.ID] = true
		if item.Label == "" || len([]rune(item.Label)) > 60 {
			return nil, fmt.Errorf("页脚链接名称须为 1–60 个字符")
		}
		switch item.Icon {
		case "github", "website", "link", "mail", "rss", "telegram", "heart", "photo", "custom":
		default:
			return nil, fmt.Errorf("页脚图标类型无效")
		}
		if !validFooterURL(item.URL, false) {
			return nil, fmt.Errorf("页脚链接须为 http(s) 地址、站内路径或 mailto 地址")
		}
		if item.Icon == "custom" && !validFooterURL(item.IconURL, true) {
			return nil, fmt.Errorf("自定义图标须填写 http(s) 图片地址或站内路径")
		}
		if item.Icon != "custom" {
			item.IconURL = ""
		}
	}
	return links, nil
}
