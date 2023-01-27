package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessURLCfg(t *testing.T) {
	cases := []struct {
		url         string
		nowS        int
		contentPart string
		cfgJSON     string
		err         string
	}{
		{
			url:         "/livesim/tsbd_1/asset.mpd",
			nowS:        0,
			contentPart: "asset.mpd",
			cfgJSON: `{
				"StartTimeS": 0,
				"TimeShiftBufferDepthS": 1,
				"StartNr": 0,
				"AvailabilityTimeCompleteFlag": true
				}`,
			err: "",
		},
		{
			url:         "/livesim/tsbd_1/tsb_asset/V300.cmfv",
			nowS:        0,
			contentPart: "tsb_asset/V300.cmfv",
			cfgJSON: `{
				"StartTimeS": 0,
				"TimeShiftBufferDepthS": 1,
				"StartNr": 0,
				"AvailabilityTimeCompleteFlag": true
				}`,
			err: "",
		},
		{
			url:         "/livesim/tsbd_a/asset.mpd",
			nowS:        0,
			contentPart: "",
			cfgJSON:     "",
			err:         `key=tsbd, err=strconv.Atoi: parsing "a": invalid syntax`,
		},
		{
			url:         "/livesim/tsbd_1",
			nowS:        0,
			contentPart: "",
			cfgJSON:     "",
			err:         "no content part",
		},
	}

	for _, c := range cases {
		urlParts := strings.Split(c.url, "/")
		cfg, idx, err := processURLCfg(urlParts, c.nowS)
		if c.err != "" {
			require.Equal(t, c.err, err.Error())
			continue
		}
		assert.NoError(t, err)
		gotContentPart := strings.Join(urlParts[idx:], "/")
		require.Equal(t, c.contentPart, gotContentPart)
		jsonBytes, err := json.MarshalIndent(cfg, "", "")
		assert.NoError(t, err)
		jsonStr := string(jsonBytes)
		wantedJSON := dedent(c.cfgJSON)
		require.Equal(t, wantedJSON, jsonStr)
	}
}

var whitespaceOnly = regexp.MustCompile("\n[ \t]+")

// dendent removes spaces and tabs right after a newline
func dedent(str string) string {
	return whitespaceOnly.ReplaceAllString(str, "\n")
}