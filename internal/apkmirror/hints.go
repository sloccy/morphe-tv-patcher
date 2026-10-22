package apkmirror

// Hints pre-fill the app page for packages we already know, so ordinary runs never search.
// They are only a first guess: the release page must name the package and the downloaded
// file is verified, and a wrong hint falls through to search. Override in settings.json.
var Hints = map[string]string{
	"com.amazon.amazonvideo.livingroom": "amazon-mobile-llc/prime-video-android-tv-android-tv",
	"com.netflix.ninja":                 "netflix-inc/netflix-android-tv",
	"com.disney.disneyplus":             "disney/disney-android-tv",
	"com.cbs.ott":                       "cbs-interactive-inc/paramount-2",
	"com.espn.score_center":             "disney/espn-android-tv",
	"com.peacocktv.peacockandroid":      "peacock-tv-llc/peacock-tv-android-tv",
	"tv.pluto.android":                  "pluto-inc/pluto-tv-android-tv",
	"com.twentyfouri.tvbridge.rte":      "rte/rte-player-android-tv",
	"com.tubitv":                        "tubi-tv/tubi-free-movies-live-tv-android-tv",
	"tv.twitch.android.app":             "twitch-interactive-inc/twitch-android-tv",
	"com.univision.prendetv":            "univision-communications-inc/vix-cine-y-tv-en-espanol-android-tv",
	"com.wbd.hbomax":                    "warnermedia-direct-llc/hbo-max-stream-movies-tv-android-tv",
	"com.formulaone.production":         "formula-one-digital-media-limited/f1-tv-android-tv",
}
