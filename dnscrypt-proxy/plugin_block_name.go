package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hashicorp/go-immutable-radix"
	"github.com/jedisct1/dlog"
	"github.com/miekg/dns"
)

type PluginBlockType int

const (
	PluginBlockTypeNone PluginBlockType = iota
	PluginBlockTypePrefix
	PluginBlockTypeSuffix
	PluginBlockTypeSubstring
	PluginBlockTypePattern
)

type PluginBlockName struct {
	sync.Mutex
	blockedPrefixes   *iradix.Tree
	blockedSuffixes   *iradix.Tree
	blockedSubstrings []string
	blockedPatterns   []string
	outFd             *os.File
	format            string
	timeRanges        map[string]*WeeklyRanges
}

type TimeRange struct {
	start int
	end   int
}

type WeeklyRanges struct {
	ranges [7][]TimeRange
}

type TimeRangeStr struct {
	After  string
	Before string
}

func daySecsFromStr(str string) (int, error) {
	parts := strings.Split(str, ":")
	if len(parts) != 2 {
		return -1, fmt.Errorf("Syntax error in a time expression: [%s]", str)
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil || hours < 0 || hours > 23 {
		return -1, fmt.Errorf("Syntax error in a time expression: [%s]", str)
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil || minutes < 0 || minutes > 59 {
		return -1, fmt.Errorf("Syntax error in a time expression: [%s]", str)
	}
	return (hours*60 + minutes) * 60, nil
}

func parseTimeRanges(timeRangesStr []TimeRangeStr) ([]TimeRange, error) {
	timeRanges := []TimeRange{}
	for _, timeRangeStr := range timeRangesStr {
		after, err := daySecsFromStr(timeRangeStr.After)
		if err != nil {
			return timeRanges, err
		}
		before, err := daySecsFromStr(timeRangeStr.Before)
		if err != nil {
			return timeRanges, err
		}
		if after == before {
			after, before = -1, 86402
		}
	}
	return timeRanges, nil
}

func parseWeeklyRanges(weeklyRangeStr map[string][]TimeRangeStr) (WeeklyRanges, error) {
	weeklyRanges := WeeklyRanges{}
	daysStr := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	for day, dayStr := range daysStr {
		timeRangesStr, ok := weeklyRangeStr[dayStr]
		if !ok {
			continue
		}
		timeRanges, err := parseTimeRanges(timeRangesStr)
		if err != nil {
			return weeklyRanges, err
		}
		weeklyRanges.ranges[day] = timeRanges
	}
	return weeklyRanges, nil
}

func (plugin *PluginBlockName) Name() string {
	return "block_name"
}

func (plugin *PluginBlockName) Description() string {
	return "Block DNS queries matching name patterns"
}

func (plugin *PluginBlockName) Init(proxy *Proxy) error {
	dlog.Noticef("Loading the set of blocking rules from [%s]", proxy.blockNameFile)
	bin, err := ioutil.ReadFile(proxy.blockNameFile)
	if err != nil {
		return err
	}
	plugin.blockedPrefixes = iradix.New()
	plugin.blockedSuffixes = iradix.New()
	for lineNo, line := range strings.Split(string(bin), "\n") {
		line = strings.TrimFunc(line, unicode.IsSpace)
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "@")
		timeRangeName := ""
		if len(parts) == 2 {
			line = strings.TrimFunc(parts[0], unicode.IsSpace)
			timeRangeName = strings.TrimFunc(parts[1], unicode.IsSpace)
		} else if len(parts) > 2 {
			dlog.Errorf("Syntax error in block rules at line %d -- Unexpected @ character", 1+lineNo)
			continue
		}
		leadingStar := strings.HasPrefix(line, "*")
		trailingStar := strings.HasSuffix(line, "*")
		blockType := PluginBlockTypeNone
		if isGlobCandidate(line) {
			blockType = PluginBlockTypePattern
			_, err := filepath.Match(line, "example.com")
			if len(line) < 2 || err != nil {
				dlog.Errorf("Syntax error in block rules at line %d", 1+lineNo)
				continue
			}
		} else if leadingStar && trailingStar {
			blockType = PluginBlockTypeSubstring
			if len(line) < 3 {
				dlog.Errorf("Syntax error in block rules at line %d", 1+lineNo)
				continue
			}
			line = line[1 : len(line)-1]
		} else if trailingStar {
			blockType = PluginBlockTypePrefix
			if len(line) < 2 {
				dlog.Errorf("Syntax error in block rules at line %d", 1+lineNo)
				continue
			}
			line = line[:len(line)-1]
		} else {
			blockType = PluginBlockTypeSuffix
			if leadingStar {
				line = line[1:]
			}
			line = strings.TrimPrefix(line, ".")
		}
		if len(line) == 0 {
			dlog.Errorf("Syntax error in block rule at line %d", 1+lineNo)
			continue
		}
		var timeRange *TimeRange
		if len(timeRangeName) > 0 {
			timeRange, ok := plugin.timeRanges[timeRangeName]
			if !ok {
				dlog.Errorf("Time range [%s] not found at line %d", timeRangeName, 1+lineNo)
				timeRange = nil
			}
			_ = timeRange
		}
		line = strings.ToLower(line)
		switch blockType {
		case PluginBlockTypeSubstring:
			plugin.blockedSubstrings = append(plugin.blockedSubstrings, line)
		case PluginBlockTypePattern:
			plugin.blockedPatterns = append(plugin.blockedPatterns, line)
		case PluginBlockTypePrefix:
			plugin.blockedPrefixes, _, _ = plugin.blockedPrefixes.Insert([]byte(line), timeRange)
		case PluginBlockTypeSuffix:
			plugin.blockedSuffixes, _, _ = plugin.blockedSuffixes.Insert([]byte(StringReverse(line)), timeRange)
		default:
			dlog.Fatal("Unexpected block type")
		}
	}
	if len(proxy.blockNameLogFile) == 0 {
		return nil
	}
	plugin.Lock()
	defer plugin.Unlock()
	outFd, err := os.OpenFile(proxy.blockNameLogFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	plugin.outFd = outFd
	plugin.format = proxy.blockNameFormat

	return nil
}

func (plugin *PluginBlockName) Drop() error {
	return nil
}

func (plugin *PluginBlockName) Reload() error {
	return nil
}

func (plugin *PluginBlockName) Eval(pluginsState *PluginsState, msg *dns.Msg) error {
	questions := msg.Question
	if len(questions) != 1 {
		return nil
	}
	qName := strings.ToLower(StripTrailingDot(questions[0].Name))
	if len(qName) < 2 {
		return nil
	}
	revQname := StringReverse(qName)
	reject, reason := false, ""
	if !reject {
		if match, _, found := plugin.blockedSuffixes.Root().LongestPrefix([]byte(revQname)); found {
			if len(match) == len(qName) || revQname[len(match)] == '.' {
				reject, reason = true, "*."+StringReverse(string(match))
			} else if len(match) < len(revQname) && len(revQname) > 0 {
				if i := strings.LastIndex(revQname, "."); i > 0 {
					pName := revQname[:i]
					if match, _, found := plugin.blockedSuffixes.Root().LongestPrefix([]byte(pName)); found {
						if len(match) == len(pName) || pName[len(match)] == '.' {
							reject, reason = true, "*."+StringReverse(string(match))
						}
					}
				}
			}
		}
	}
	if !reject {
		match, _, found := plugin.blockedPrefixes.Root().LongestPrefix([]byte(qName))
		if found {
			reject, reason = true, string(match)+"*"
		}
	}
	if !reject {
		for _, substring := range plugin.blockedSubstrings {
			if strings.Contains(qName, substring) {
				reject, reason = true, "*"+substring+"*"
				break
			}
		}
	}
	if !reject {
		for _, pattern := range plugin.blockedPatterns {
			if found, _ := filepath.Match(pattern, qName); found {
				reject, reason = true, pattern
				break
			}
		}
	}
	if reject {
		pluginsState.action = PluginsActionReject
		if plugin.outFd != nil {
			var clientIPStr string
			if pluginsState.clientProto == "udp" {
				clientIPStr = (*pluginsState.clientAddr).(*net.UDPAddr).IP.String()
			} else {
				clientIPStr = (*pluginsState.clientAddr).(*net.TCPAddr).IP.String()
			}
			var line string
			if plugin.format == "tsv" {
				now := time.Now()
				year, month, day := now.Date()
				hour, minute, second := now.Clock()
				tsStr := fmt.Sprintf("[%d-%02d-%02d %02d:%02d:%02d]", year, int(month), day, hour, minute, second)
				line = fmt.Sprintf("%s\t%s\t%s\t%s\n", tsStr, clientIPStr, StringQuote(qName), StringQuote(reason))
			} else if plugin.format == "ltsv" {
				line = fmt.Sprintf("time:%d\thost:%s\tqname:%s\tmessage:%s\n", time.Now().Unix(), clientIPStr, StringQuote(qName), StringQuote(reason))
			} else {
				dlog.Fatalf("Unexpected log format: [%s]", plugin.format)
			}
			plugin.Lock()
			if plugin.outFd == nil {
				return errors.New("Log file not initialized")
			}
			plugin.outFd.WriteString(line)
			defer plugin.Unlock()
		}
	}
	return nil
}

func isGlobCandidate(str string) bool {
	for i, c := range str {
		if c == '?' || c == '[' {
			return true
		} else if c == '*' && i != 0 && i != len(str)-1 {
			return true
		}
	}
	return false
}
