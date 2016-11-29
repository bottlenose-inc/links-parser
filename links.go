package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asaskevich/govalidator"
	rj "github.com/bottlenose-inc/rapidjson" // faster json handling
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

var (
	redirects = []*regexp.Regexp{
		regexp.MustCompile(`\Ahttp://adf.ly/[0-9]*/([\.0-9a-zA-Z:/-]*)`),
		regexp.MustCompile(`\Ahttp://weightless.mysharebar.com/view[?]iframe=([\.0-9a-zA-Z:/-]*)`),
	}
)

const (
	CONTENT_LENGTH_LIMIT_BYTES = 1024 * 512 // 512 MB
)

func Links(w http.ResponseWriter, r *http.Request) {
	body, err := GetRequests(w, r)
	if err != nil {
		incUnsuccessfulCounter()
		logger.Error("Error in GetRequests call")
		return
	}
	requestJson, err := rj.NewParsedJson(body)
	defer requestJson.Free()
	if err != nil {
		invalidRequestsCounter.Inc()
		logger.Warning("Client request was invalid JSON: "+err.Error(), map[string]string{"body": string(body)})
		SendErrorResponse(w, "Unable to parse request - invalid JSON detected", http.StatusBadRequest)
		return
	}
	requestCt := requestJson.GetContainer()
	if requestCt.GetType() == rj.TypeNull {
		return
	}
	requests := requestCt.GetMemberOrNil("request").GetArrayOrNil()
	if len(requests) == 0 {
		invalidRequestsCounter.Inc()
		logger.Warning("Client request was invalid JSON - missing/empty request array")
		SendErrorResponse(w, "Unable to parse request - invalid JSON detected", http.StatusBadRequest)
		return
	}

	respCode := http.StatusOK
	responses := rj.NewDoc()
	defer responses.Free()
	responsesCt := responses.GetContainerNewObj()
	var responsesArray []*rj.Container
	for _, request := range requests {
		response := responses.NewContainerObj()
		req, err := request.GetMember("url")
		if err != nil {
			response.AddValue("error", "Missing url key")
			//logger.Error("Request missing URL key: " + request.String())
			responsesArray = append(responsesArray, response)
			respCode = http.StatusBadRequest
			incUnsuccessfulCounter()
			continue
		}

		// parse request URL, create hash for redis
		reqStr, err := req.GetString()
		reqStr = CheckRedirectURL(reqStr)
		u, err := url.Parse(reqStr)
		if err != nil {
			response.AddValue("error", "URL parse error")
			logger.Warning("url Parse error: " + reqStr)
			respCode = http.StatusNonAuthoritativeInfo
			incUnsuccessfulCounter()
			continue
		}
		rootUrl := u.Host + u.Path
		u.RawQuery = CleanQuery(u)
		if u.RawQuery != "" {
			rootUrl = rootUrl + "?" + u.RawQuery
		}
		hash := fmt.Sprintf("%x", md5.Sum([]byte(rootUrl)))

		// check redis
		respStr, err := redisClient.Get(hash).Result()
		if err == nil {
			cachedJson, _ := rj.NewParsedStringJson(respStr)
			defer cachedJson.Free()
			response.SetContainer(cachedJson.GetContainer())
			response.AddValue("cacheHit", true)
			incCacheHitCounter()
		} else {
			err = FetchUrl(reqStr, u, rootUrl, 0, response)

			if err != nil {
				logger.Warning("FetchUrl fail: " + err.Error())
				incUnsuccessfulCounter()
				response.AddValue("error", err.Error())
				respCode = http.StatusNonAuthoritativeInfo
				redisErr := redisClient.Set(hash, response.String(), cfg.RedisErrorTTL).Err()
				if redisErr != nil {
					logger.Error("Error saving response in Redis: " + redisErr.Error())
				}
			} else {
				err = redisClient.Set(hash, response.String(), cfg.RedisTTL).Err()
				if err != nil {
					logger.Error("Error saving response in Redis: " + err.Error())
				}

				response.AddValue("cacheHit", false)
				incCacheMissCounter()
			}
		}

		errored := response.HasMember("error")
		if errored {
			responsesArray = append(responsesArray, response)
		} else {
			incSuccessfulCounter()

			link := responses.NewContainerObj()
			link.AddMember("link", response)
			responsesArray = append(responsesArray, link)
		}

		logProcessed()
	}

	// Send response
	responsesCt.AddMemberArray("response", responsesArray)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var b bytes.Buffer
	b.Write(responses.Bytes())

	w.WriteHeader(respCode)
	_, err = b.WriteTo(w)
	if err != nil {
		// Should not run into this error...
		logger.Error("Error encoding response: "+err.Error(), map[string]string{"response": responses.String()})
	}
}

func FetchUrl(req string, u *url.URL, rootUrl string, redirectCount int, response *rj.Container) error {
	start := time.Now()

	// check blacklist
	if IsBlacklisted(rootUrl) {
		return errors.New("Invalid URL (blacklisted)")
	}

	result, err := httpClient.Get(u.String())
	if result != nil {
		defer result.Body.Close()
	}
	if err != nil {
		if urlError, ok := err.(*url.Error); ok && urlError.Err == RedirectAttempted {
			nextU, err := result.Location()
			if err != nil {
				logger.Error("url Parse error: " + req)
				incUnsuccessfulCounter()
				return err
			}
			redirect := CheckRedirectURL(nextU.String())
			nextU, err = url.Parse(redirect)
			if err != nil {
				logger.Error("url Parse error: " + redirect)
				incUnsuccessfulCounter()
				return err
			}
			if nextU.Scheme == "" {
				nextU.Scheme = u.Scheme
			}
			if nextU.Host == "" {
				nextU.Host = u.Host
			}
			rootUrl = nextU.Host + nextU.Path
			if nextU.RawQuery != "" {
				rootUrl = rootUrl + "?" + nextU.RawQuery
			}

			if redirectCount >= cfg.MaxRedirect {
				return errors.New("Max redirects limit reached! Request URL: " + req)
			} else {
				return FetchUrl(req, nextU, rootUrl, redirectCount+1, response)
			}
		} else {
			return err
		}
	}

	// check result status code
	if result.StatusCode != 200 {
		return errors.New("HTTP GET result status code: " + strconv.Itoa(result.StatusCode) + " url: " + u.String())
	} else {
		link, hasLink := result.Header["Link"]
		if hasLink {
			if redirect := HeaderLinkRedirect(link); redirect != "" {
				if redirectCount >= cfg.MaxRedirect {
					return errors.New("Max redirects limit reached! Request URL: " + req)
				} else {
					nextU, err := url.Parse(redirect)
					if err != nil {
						logger.Error("url Parse error: " + redirect)
						incUnsuccessfulCounter()
						return err
					}
					if nextU.Scheme == "" {
						nextU.Scheme = u.Scheme
					}
					if nextU.Host == "" {
						nextU.Host = u.Host
					}
					rootUrl = nextU.Host + nextU.Path
					if nextU.RawQuery != "" {
						rootUrl = rootUrl + "?" + nextU.RawQuery
					}
					return FetchUrl(req, nextU, rootUrl, redirectCount+1, response)
				}
			}
		}
	}

	// check result length
	if result.ContentLength > CONTENT_LENGTH_LIMIT_BYTES {
		return errors.New("File at URL is too large")
	}

	// check for text result
	contentType, hasType := result.Header["Content-Type"]
	if hasType {
		contentType := strings.Join(contentType, " ")
		contentType = strings.ToLower(contentType)
		if !strings.Contains(contentType, "text") {
			return errors.New("Invalid content-type detected: " + contentType)
		}
	}

	rootUrl = strings.ToLower(strings.TrimRight(rootUrl, "/"))
	response.AddValue("fetchDuration", int(time.Now().Sub(start).Seconds()*1000))
	response.AddValue("originalUrl", req)
	response.AddValue("rootUrl", rootUrl)
	response.AddValue("id", rootUrl)
	response.AddValue("url", u.String())
	response.AddValue("providerUrl", "http://"+u.Host)

	// check result encoding
	var resultReader io.ReadCloser
	switch result.Header.Get("Content-Encoding") {
	case "gzip":
		resultReader, err = gzip.NewReader(result.Body)
		defer resultReader.Close()
		if err != nil {
			logger.Error("gzip error: " + err.Error())
			return errors.New("gzip error: " + err.Error())
		}
	default:
		resultReader = result.Body
	}

	// parse response
	utf8Reader, err := charset.NewReader(resultReader, "")
	if err != nil {
		return err
	}
	body := html.NewTokenizer(utf8Reader)

	start = time.Now()
	tags := make(map[string]string)
	jsRedirect := ParseBody(body, tags, u.Host)
	if jsRedirect != "" {
		nextUrl := strings.Replace(jsRedirect, "\\", "", -1)
		nextU, err := url.Parse(nextUrl)
		if err != nil {
			logger.Error("url Parse error: " + req)
			incUnsuccessfulCounter()
			return err
		}
		if nextU.Scheme == "" {
			nextU.Scheme = u.Scheme
		}
		if nextU.Host == "" {
			nextU.Host = u.Host
		}
		rootUrl = nextU.Host + nextU.Path

		if redirectCount >= cfg.MaxRedirect {
			return errors.New("Max redirects limit reached! Request URL: " + req)
		} else {
			return FetchUrl(req, nextU, rootUrl, redirectCount+1, response)
		}
	}

	// check canonical URL
	canonical, hasCanonical := tags["canonical"]
	if hasCanonical {
		canonicalUrl, err := url.Parse(canonical)
		if err == nil {
			if canonicalUrl.Host == u.Host {
				rootUrl = canonicalUrl.Host + canonicalUrl.Path
				if canonicalUrl.RawQuery != "" {
					rootUrl = rootUrl + "?" + canonicalUrl.RawQuery
				}
				rootUrl = strings.ToLower(strings.TrimRight(rootUrl, "/"))
				response.SetMemberValue("rootUrl", rootUrl)
				response.SetMemberValue("id", rootUrl)
				response.SetMemberValue("url", canonicalUrl.String())
				response.SetMemberValue("providerUrl", "http://"+canonicalUrl.Host)
			}
		}
	}

	// title, name, description
	title, hasOG := tags["og:title"]
	if !hasOG {
		title = tags["title"]
	}

	providerName := IdentifyProviderName(u.Host, tags["title"], title)
	response.AddValue("providerName", providerName)

	response.AddValue("title", TrimDescription(strings.TrimSpace(IdentifyTitle(title, providerName))))

	linkType, hasOG := tags["og:type"]
	if hasOG {
		response.AddValue("type", linkType)
	} else {
		response.AddValue("type", "website")
	}
	desc, hasOG := tags["og:description"]
	if hasOG {
		response.AddValue("description", TrimDescription(desc))
	} else {
		desc, hasDesc := tags["description"]
		if hasDesc {
			response.AddValue("description", TrimDescription(desc))
		}
	}
	image, hasOG := tags["og:image"]
	if hasOG {
		imageUrl, err := url.Parse(image)
		if err != nil {
			logger.Warning("Image URL parse fail: " + err.Error())
		} else {
			resolved := u.ResolveReference(imageUrl)
			scheme := resolved.Scheme
			if scheme == "http" || scheme == "https" || len(resolved.String()) < cfg.MaxImgURL {
				response.AddValue("imageUrl", resolved.String())
			}
		}
	}

	// keywords
	keywords := make(map[string]bool)
	for _, tag := range cfg.KeywordsTags {
		words, hasTag := tags[tag]
		if hasTag {
			splitter := strings.Replace(words, ";", "////", -1)
			splitter = strings.Replace(splitter, ",", "////", -1)
			for _, word := range strings.Split(splitter, "////") {
				if trimmed := strings.TrimSpace(word); trimmed != "" {
					keywords[strings.TrimSpace(word)] = true
				}
			}
		}
	}
	if len(keywords) > 0 {
		response.AddValue("providerKeywords", nil)
		keywordsArray, _ := response.GetMember("providerKeywords")
		keywordsArray.InitArray()
		for word := range keywords {
			keywordsArray.ArrayAppend(word)
		}
	}

	// favicon
	favicon, hasFavicon := tags["favicon"]
	response.AddValue("favicon", nil)
	if hasFavicon {
		if !govalidator.IsURL(favicon) {
			favUrl, err := url.Parse(favicon)
			if err != nil {
				logger.Warning("Favion URL parse fail: " + err.Error())
			} else {
				resolved := u.ResolveReference(favUrl)
				scheme := resolved.Scheme
				if scheme == "http" || scheme == "https" || len(resolved.String()) < cfg.MaxImgURL {
					response.SetMemberValue("favicon", resolved.String())
				}
			}
		} else {
			response.SetMemberValue("favicon", favicon)
		}
	}
	response.AddValue("parseDuration", int(time.Now().Sub(start).Seconds()*1000))

	return nil
}

// HTML parsing based on html.Tokenizer
func ParseBody(body *html.Tokenizer, tags map[string]string, host string) string {
	iconSet := false
	for body != nil {
		tt := body.Next()
		switch tt {
		case html.ErrorToken:
			// end of body
			return ""
		case html.SelfClosingTagToken:
			fallthrough
		case html.StartTagToken:
			t := body.Token()

			switch t.Data {
			// specific js handling
			case "script":
				if host == "thr.cm" {
					body.Next()
					js := string(body.Text())

					i := strings.Index(js, "window.location.replace")
					if i == -1 {
						continue
					}
					i = i + 25
					redirect := js[i:]
					i = strings.Index(redirect, "'")
					if i == -1 {
						continue
					}
					return redirect[:i]
				}
			// look for title, description, OG values in meta tags
			case "meta":
				tag, content := "", ""
				for _, attr := range t.Attr {
					key := strings.ToLower(attr.Key)
					if key == "name" {
						tag = strings.ToLower(attr.Val)
					} else if key == "property" {
						tag = strings.ToLower(attr.Val)
					} else if key == "content" {
						content = html.UnescapeString(attr.Val)
					}
				}
				if tag != "" && content != "" {
					if _, isMulti := cfg.MultiTagsMap[tag]; isMulti {
						val := FixEncoding(content)
						tags[tag] = tags[tag] + ";" + val
					} else {
						tags[tag] = FixEncoding(content)
					}
				}
			// look for favicon in link tags
			case "link":
				tag, content := "", ""
				for _, attr := range t.Attr {
					key := strings.ToLower(attr.Key)
					if key == "rel" {
						tag = strings.ToLower(attr.Val)
					} else if key == "href" {
						content = attr.Val
					}
				}
				if (tag == "icon" || tag == "shortcut icon") && content != "" && !iconSet {
					tags["favicon"] = content
					iconSet = true
				}
				if tag == "canonical" && content != "" {
					tags["canonical"] = content
				}
			// title text in next token
			case "title":
				body.Next()
				text := string(body.Text())
				tags["title"] = FixEncoding(text)
			}
		}
	}
	return ""
}

func IsBlacklisted(rootUrl string) bool {
	for _, a := range cfg.Blacklist {
		if strings.Contains(rootUrl, a) {
			return true
		}
	}
	return false
}

func CheckRedirectURL(u string) string {
	//u = strings.ToLower(u)
	for _, re := range redirects {
		if redirect := re.FindStringSubmatch(u); redirect != nil {
			return redirect[1]
		}
	}
	return u
}

func HeaderLinkRedirect(link []string) string {
	link = strings.Split(link[0], ";")
	if len(link) != 2 {
		return ""
	}
	if strings.TrimSpace(link[1]) == "rel=\"canonical\"" {
		return strings.TrimRight(strings.TrimLeft(strings.TrimSpace(link[0]), "<"), ">")
	} else {
		return ""
	}
}

// trim descriptions
func TrimDescription(desc string) string {
	result := strings.TrimSpace(desc)
	count := cfg.DescMaxWords
	for i := range result {
		if result[i] == ' ' {
			count = count - 1
			if count == 0 {
				return result[0:i] + "…"
			}
		}
		if i >= cfg.DescMaxChars {
			return result[0:i] + "…"
		}
	}
	return result
}

// strip utm parameters
func CleanQuery(u *url.URL) string {
	q := u.Query()
	for key := range q {
		if strings.HasPrefix(key, "utm") {
			q.Del(key)
		}
	}
	return q.Encode()
}

// fix bad encoding
func FixEncoding(text string) string {
	result := strings.Replace(text, "â€™", "’", -1)
	result = strings.Replace(result, "â‚¬", "€", -1)
	result = strings.Replace(result, "â€š", "‚", -1)
	result = strings.Replace(result, "â€ž", "„", -1)
	result = strings.Replace(result, "â€¦", "…", -1)
	result = strings.Replace(result, "â€°", "‰", -1)
	result = strings.Replace(result, "â€¹", "‹", -1)
	result = strings.Replace(result, "â€˜", "‘", -1)
	result = strings.Replace(result, "â€œ", "“", -1)
	result = strings.Replace(result, "â€¢", "•", -1)
	result = strings.Replace(result, "â€“", "–", -1)
	result = strings.Replace(result, "â€”", "—", -1)
	result = strings.Replace(result, "â„¢", "™", -1)
	result = strings.Replace(result, "â€º", "›", -1)

	result = strings.Replace(result, "Ë†", "ˆ", -1)
	result = strings.Replace(result, "â€", "†", -1)
	result = strings.Replace(result, "Æ’", "ƒ", -1)
	result = strings.Replace(result, "Å’", "Œ", -1)
	result = strings.Replace(result, "Å½", "Ž", -1)
	result = strings.Replace(result, "â€", "”", -1)
	result = strings.Replace(result, "Ëœ", "˜", -1)
	result = strings.Replace(result, "Å“", "œ", -1)
	result = strings.Replace(result, "Å¾", "ž", -1)
	result = strings.Replace(result, "Å¸", "Ÿ", -1)
	result = strings.Replace(result, "Å¡", "š", -1)
	result = strings.Replace(result, "Â¡", "¡", -1)
	result = strings.Replace(result, "Â¢", "¢", -1)
	result = strings.Replace(result, "Â£", "£", -1)
	result = strings.Replace(result, "Â¤", "¤", -1)
	result = strings.Replace(result, "Â¥", "¥", -1)
	result = strings.Replace(result, "Â¦", "¦", -1)
	result = strings.Replace(result, "Â§", "§", -1)
	result = strings.Replace(result, "Â¨", "¨", -1)
	result = strings.Replace(result, "Â©", "©", -1)
	result = strings.Replace(result, "Âª", "ª", -1)
	result = strings.Replace(result, "Â«", "«", -1)
	result = strings.Replace(result, "Â¬", "¬", -1)
	result = strings.Replace(result, "Â­", " ", -1)
	result = strings.Replace(result, "Â®", "®", -1)
	result = strings.Replace(result, "Â¯", "¯", -1)
	result = strings.Replace(result, "Â°", "°", -1)
	result = strings.Replace(result, "Â±", "±", -1)
	result = strings.Replace(result, "Â²", "²", -1)
	result = strings.Replace(result, "Â³", "³", -1)
	result = strings.Replace(result, "Â´", "´", -1)
	result = strings.Replace(result, "Âµ", "µ", -1)
	result = strings.Replace(result, "Â¶", "¶", -1)
	result = strings.Replace(result, "Â·", "·", -1)
	result = strings.Replace(result, "Â¸", "¸", -1)
	result = strings.Replace(result, "Â¹", "¹", -1)
	result = strings.Replace(result, "Âº", "º", -1)
	result = strings.Replace(result, "Â»", "»", -1)
	result = strings.Replace(result, "Â¼", "¼", -1)
	result = strings.Replace(result, "Â½", "½", -1)
	result = strings.Replace(result, "Â¾", "¾", -1)
	result = strings.Replace(result, "Â¿", "¿", -1)
	result = strings.Replace(result, "ÃŽ", "Î", -1)
	result = strings.Replace(result, "Ã", "Ï", -1)
	result = strings.Replace(result, "Ã", "Ð", -1)
	result = strings.Replace(result, "Ã‘", "Ñ", -1)
	result = strings.Replace(result, "Ã’", "Ò", -1)
	result = strings.Replace(result, "Ã“", "Ó", -1)
	result = strings.Replace(result, "Ã”", "Ô", -1)
	result = strings.Replace(result, "Ã•", "Õ", -1)
	result = strings.Replace(result, "Ã–", "Ö", -1)
	result = strings.Replace(result, "Ã—", "×", -1)
	result = strings.Replace(result, "Ã˜", "Ø", -1)
	result = strings.Replace(result, "Ã™", "Ù", -1)
	result = strings.Replace(result, "Ãš", "Ú", -1)
	result = strings.Replace(result, "Ã›", "Û", -1)
	result = strings.Replace(result, "Ãœ", "Ü", -1)
	result = strings.Replace(result, "Ã", "Ý", -1)
	result = strings.Replace(result, "Ãž", "Þ", -1)
	result = strings.Replace(result, "ÃŸ", "ß", -1)
	result = strings.Replace(result, "Ã", "à", -1)
	result = strings.Replace(result, "Ã¡", "á", -1)
	result = strings.Replace(result, "Ã¢", "â", -1)
	result = strings.Replace(result, "Ã£", "ã", -1)
	result = strings.Replace(result, "Ã¤", "ä", -1)
	result = strings.Replace(result, "Ã¥", "å", -1)
	result = strings.Replace(result, "Ã¦", "æ", -1)
	result = strings.Replace(result, "Ã§", "ç", -1)
	result = strings.Replace(result, "Ã¨", "è", -1)
	result = strings.Replace(result, "Ã©", "é", -1)
	result = strings.Replace(result, "Ãª", "ê", -1)
	result = strings.Replace(result, "Ã«", "ë", -1)
	result = strings.Replace(result, "Ã¬", "ì", -1)
	result = strings.Replace(result, "Ã­", "í", -1)
	result = strings.Replace(result, "Ã®", "î", -1)
	result = strings.Replace(result, "Ã¯", "ï", -1)
	result = strings.Replace(result, "Ã°", "ð", -1)
	result = strings.Replace(result, "Ã±", "ñ", -1)
	result = strings.Replace(result, "Ã²", "ò", -1)
	result = strings.Replace(result, "Ã³", "ó", -1)
	result = strings.Replace(result, "Ã´", "ô", -1)
	result = strings.Replace(result, "Ãµ", "õ", -1)
	result = strings.Replace(result, "Ã¶", "ö", -1)
	result = strings.Replace(result, "Ã·", "÷", -1)
	result = strings.Replace(result, "Ã¸", "ø", -1)
	result = strings.Replace(result, "Ã¹", "ù", -1)
	result = strings.Replace(result, "Ãº", "ú", -1)
	result = strings.Replace(result, "Ã»", "û", -1)
	result = strings.Replace(result, "Ã¼", "ü", -1)
	result = strings.Replace(result, "Ã½", "ý", -1)
	result = strings.Replace(result, "Ã¾", "þ", -1)
	result = strings.Replace(result, "Ã¿", "ÿ", -1)
	result = strings.Replace(result, "Ã€", "À", -1)
	result = strings.Replace(result, "Ã", "Á", -1)
	result = strings.Replace(result, "Ã‚", "Â", -1)
	result = strings.Replace(result, "Ãƒ", "Ã", -1)
	result = strings.Replace(result, "Ã„", "Ä", -1)
	result = strings.Replace(result, "Ã…", "Å", -1)
	result = strings.Replace(result, "Ã†", "Æ", -1)
	result = strings.Replace(result, "Ã‡", "Ç", -1)
	result = strings.Replace(result, "Ãˆ", "È", -1)
	result = strings.Replace(result, "Ã‰", "É", -1)
	result = strings.Replace(result, "ÃŠ", "Ê", -1)
	result = strings.Replace(result, "Ã‹", "Ë", -1)
	result = strings.Replace(result, "ÃŒ", "Ì", -1)

	result = strings.Replace(result, "Â", "", -1)
	result = strings.Replace(result, "Å", "Š", -1)
	result = strings.Replace(result, "Ã", "Í", -1)

	return result
}

// provider name either from title/OG title, or URL
func IdentifyProviderName(providerUrl string, fullTitle string, ogTitle string) string {
	providerUrl = strings.ToLower(providerUrl)
	if knownName, known := ProviderNames[providerUrl]; known {
		return knownName
	}
	if strings.HasPrefix(providerUrl, "www.") {
		providerUrl = providerUrl[4:]
	}
	if knownName, known := ProviderNames[providerUrl]; known {
		return knownName
	}

	if ogTitle != "" && fullTitle != ogTitle {
		parts := strings.Split(fullTitle, " - ")
		if strings.Contains(fullTitle, "|") {
			parts = strings.Split(fullTitle, "|")
		}

		if len(parts) == 2 {
			if strings.TrimSpace(ogTitle) == strings.TrimSpace(parts[0]) {
				return strings.TrimSpace(parts[1])
			}
			if strings.TrimSpace(ogTitle) == strings.TrimSpace(parts[1]) {
				return strings.TrimSpace(parts[0])
			}
		}
	}

	re := regexp.MustCompile("([a-z0-9-]+)[.co|.com|.ne|.net|.org]*.[a-zA-Z]+$")
	provider := re.FindStringSubmatch(strings.ToLower(providerUrl))
	if provider != nil {
		name := Capitalize(provider[1])
		if name == "In" {
			name = providerUrl
			if strings.HasPrefix(name, "www.") {
				name = name[4:]
			}
		}
		return name
	}

	return ""
}

func Capitalize(s string) string {
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// strip provider name from title
func IdentifyTitle(title string, providerName string) string {
	parts := strings.Split(title, "-")
	joinChar := "-"
	if strings.Contains(title, "|") {
		parts = strings.Split(title, "|")
		joinChar = "|"
	}

	if len(parts) > 1 {
		if strings.TrimSpace(strings.ToLower(strings.Replace(parts[0], " ", "", -1))) == strings.ToLower(providerName) {
			trimmed := strings.Join(parts[1:], joinChar)
			return strings.TrimSpace(trimmed)
		}
		if strings.TrimSpace(strings.ToLower(strings.Replace(parts[len(parts)-1], " ", "", -1))) == strings.ToLower(providerName) {
			trimmed := strings.Join(parts[0:len(parts)-1], joinChar)
			return strings.TrimSpace(trimmed)
		}
	}
	return title
}
