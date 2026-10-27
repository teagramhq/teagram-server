package stickervalidation

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const maxTGSJSONNesting = 64

type tgsValueContext uint8

const (
	tgsValueDefault tgsValueContext = iota
	tgsValueSplitPosition
	tgsValueEasingHandleCandidate
	tgsValueEasingCoordinate
)

var errInvalidTGS = errors.New("invalid TGS")

func validateTGS(data []byte, _ Target) (int, int, time.Duration, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, 0, errInvalidTGS
	}
	inflated, readErr := io.ReadAll(io.LimitReader(reader, maxTGSInflatedBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(inflated) > maxTGSInflatedBytes {
		return 0, 0, 0, errInvalidTGS
	}

	width, height, frameRate, inPoint, outPoint, err := parseTGSJSON(inflated)
	if err != nil || frameRate <= 0 || frameRate > 60 || outPoint <= inPoint {
		return 0, 0, 0, errInvalidTGS
	}
	seconds := (outPoint - inPoint) / frameRate
	if !isFinite(seconds) || seconds <= 0 || seconds > 3 {
		return 0, 0, 0, errInvalidTGS
	}
	return width, height, time.Duration(math.Round(seconds * float64(time.Second))), nil
}

func parseTGSJSON(data []byte) (int, int, float64, float64, float64, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return 0, 0, 0, 0, 0, errInvalidTGS
	}

	var (
		tgsSeen       bool
		widthSeen     bool
		heightSeen    bool
		frameRateSeen bool
		inPointSeen   bool
		outPointSeen  bool
		assetsSeen    bool
		tgsVersion    int64
		width         int64
		height        int64
		frameRate     float64
		inPoint       float64
		outPoint      float64
	)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return 0, 0, 0, 0, 0, errInvalidTGS
		}
		switch key {
		case "tgs":
			if tgsSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			tgsSeen = true
			tgsVersion, err = tgsInteger(decoder)
		case "w":
			if widthSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			widthSeen = true
			width, err = tgsInteger(decoder)
		case "h":
			if heightSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			heightSeen = true
			height, err = tgsInteger(decoder)
		case "fr":
			if frameRateSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			frameRateSeen = true
			frameRate, err = tgsNumber(decoder)
		case "ip":
			if inPointSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			inPointSeen = true
			inPoint, err = tgsNumber(decoder)
		case "op":
			if outPointSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			outPointSeen = true
			outPoint, err = tgsNumber(decoder)
		case "assets":
			if assetsSeen {
				return 0, 0, 0, 0, 0, errInvalidTGS
			}
			assetsSeen = true
			err = scanTGSAssets(decoder, 2)
		default:
			context := tgsValueDefault
			if key == "x" {
				context = tgsValueSplitPosition
			}
			err = scanTGSValue(decoder, 2, false, context)
		}
		if err != nil {
			return 0, 0, 0, 0, 0, errInvalidTGS
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return 0, 0, 0, 0, 0, errInvalidTGS
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return 0, 0, 0, 0, 0, errInvalidTGS
	}
	if !tgsSeen || !widthSeen || !heightSeen || !frameRateSeen || !inPointSeen || !outPointSeen || tgsVersion != 1 {
		return 0, 0, 0, 0, 0, errInvalidTGS
	}
	if width < 1 || width > 1<<31-1 || height < 1 || height > 1<<31-1 {
		return 0, 0, 0, 0, 0, errInvalidTGS
	}
	return int(width), int(height), frameRate, inPoint, outPoint, nil
}

func tgsInteger(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, errInvalidTGS
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, errInvalidTGS
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, errInvalidTGS
	}
	return value, nil
}

func tgsNumber(decoder *json.Decoder) (float64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, errInvalidTGS
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, errInvalidTGS
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || !isFinite(value) {
		return 0, errInvalidTGS
	}
	return value, nil
}

func scanTGSAssets(decoder *json.Decoder, depth int) error {
	if depth > maxTGSJSONNesting {
		return errInvalidTGS
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return errInvalidTGS
	}
	for decoder.More() {
		if err := scanTGSValue(decoder, depth+1, true, tgsValueDefault); err != nil {
			return errInvalidTGS
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return errInvalidTGS
	}
	return nil
}

func scanTGSValue(decoder *json.Decoder, depth int, assetObject bool, context tgsValueContext) error {
	if depth > maxTGSJSONNesting {
		return errInvalidTGS
	}
	token, err := decoder.Token()
	if err != nil {
		return errInvalidTGS
	}
	if context == tgsValueSplitPosition && token != json.Delim('{') {
		return errInvalidTGS
	}
	if context == tgsValueEasingCoordinate {
		return scanTGSEasingCoordinate(decoder, token)
	}
	if context == tgsValueEasingHandleCandidate && token != json.Delim('{') {
		context = tgsValueDefault
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || assetObject && key == "p" {
					return errInvalidTGS
				}
				childContext := tgsValueDefault
				switch {
				case context == tgsValueEasingHandleCandidate && key == "x":
					childContext = tgsValueEasingCoordinate
				case key == "x":
					childContext = tgsValueSplitPosition
				case key == "i" || key == "o":
					childContext = tgsValueEasingHandleCandidate
				}
				if err := scanTGSValue(decoder, depth+1, false, childContext); err != nil {
					return errInvalidTGS
				}
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
				return errInvalidTGS
			}
		case '[':
			for decoder.More() {
				if err := scanTGSValue(decoder, depth+1, false, tgsValueDefault); err != nil {
					return errInvalidTGS
				}
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
				return errInvalidTGS
			}
		default:
			return errInvalidTGS
		}
	case string:
		if strings.HasPrefix(strings.ToLower(value), "data:") {
			return errInvalidTGS
		}
	}
	return nil
}

func scanTGSEasingCoordinate(decoder *json.Decoder, token json.Token) error {
	if number, ok := token.(json.Number); ok {
		value, err := strconv.ParseFloat(number.String(), 64)
		if err == nil && isFinite(value) {
			return nil
		}
		return errInvalidTGS
	}
	if token != json.Delim('[') {
		return errInvalidTGS
	}
	count := 0
	for decoder.More() {
		token, err := decoder.Token()
		number, ok := token.(json.Number)
		if err != nil || !ok {
			return errInvalidTGS
		}
		value, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || !isFinite(value) {
			return errInvalidTGS
		}
		count++
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') || count == 0 {
		return errInvalidTGS
	}
	return nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
