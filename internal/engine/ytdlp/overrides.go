package ytdlp

// ResidualOverrides maps each extractor name in extractors_residual.txt to the
// routing specs that claim its URIs. A spec is "host" or "host/fragment":
// the host is a lowercase label-boundary suffix (no port), and the optional
// fragment must appear in the URI's decoded path — the path scoping keeps
// whole-host claims off shared infrastructure: a SharePoint document or an
// archived non-YouTube page must still reach the plain-download lane, since
// the residual extractor never claimed it. It is hand-maintained: the
// generator tells you which names need entries on each regen, and the
// coverage test fails if any residual name lacks one.
var ResidualOverrides = map[string][]string{
	"Allstar":            {"allstar.gg"},
	"AltCensoredChannel": {"altcensored.com"},
	"Audiomack":          {"audiomack.com"},
	"AudiomackAlbum":     {"audiomack.com"},
	"Audius":             {"audius.co"},
	"BBCCoUk":            {"bbc.co.uk"},
	"BYUtv":              {"byutv.org"},
	"BandcampUser":       {"bandcamp.com"},
	"CBC":                {"cbc.ca"},
	"CBCPlayerPlaylist":  {"cbc.ca"},
	"CBSSportsEmbed":     {"cbssports.com", "247sports.com"},
	"CNN":                {"cnn.com"},
	"CTVNews":            {"ctvnews.ca"},
	"DLFCorpus":          {"deutschlandfunk.de"},
	"DLiveStream":        {"dlive.tv"},
	// Dailymotion claims only video/player/swf paths on lequipe.fr — the
	// rest of the host is news pages, which stay on the plain lane.
	"Dailymotion":           {"dailymotion.com", "dai.ly", "lequipe.fr/video/", "lequipe.fr/player", "lequipe.fr/swf/"},
	"DailymotionUser":       {"dailymotion.com", "dai.ly"},
	"DangalPlay":            {"dangalplay.com"},
	"DiscoveryPlus":         {"discoveryplus.com"},
	"EpiconSeries":          {"epicon.in"},
	"FoxNewsArticle":        {"foxnews.com"},
	"GetCourseRu":           {"getcourse.ru", "getcourse.io", "academymel.online", "mani-beauty.com", "psbook.ru"},
	"GloboArticle":          {"globo.com"},
	"GoodGame":              {"goodgame.ru"},
	"HotStar":               {"hotstar.com"},
	"HuyaLive":              {"huya.com"},
	"IPrima":                {"iprima.cz"},
	"IdagioPlaylist":        {"idagio.com"},
	"IdagioRecording":       {"idagio.com"},
	"ImdbList":              {"imdb.com/list/ls"},
	"Imgur":                 {"imgur.com"},
	"Instagram":             {"instagram.com"},
	"IviCompilation":        {"ivi.ru"},
	"Kick":                  {"kick.com"},
	"KnownDRM":              knownDRMHosts,
	"LePlaylist":            {"le.com"},
	"Mediaite":              {"mediaite.com"},
	"MindsChannel":          {"minds.com"},
	"Mixcloud":              {"mixcloud.com"},
	"Mxplayer":              {"mxplayer.in"},
	"NBA":                   {"nba.com"},
	"NBAWatch":              {"nba.com"},
	"NBCSports":             {"nbcsports.com"},
	"NRKPlaylist":           {"nrk.no"},
	"NTVCoJpCU":             {"ntv.co.jp"},
	"NYTimesArticle":        {"nytimes.com"},
	"NebulaChannel":         {"watchnebula.com", "nebula.app", "nebula.tv"},
	"NebulaClass":           {"watchnebula.com", "nebula.app", "nebula.tv"},
	"NekoHacker":            {"nekohacker.com"},
	"Omnyfm":                {"omny.fm"},
	"OmnyfmPlaylist":        {"omny.fm"},
	"PartiLivestream":       {"parti.com"},
	"PatreonCampaign":       {"patreon.com"},
	"PornHubPagedVideoList": pornhubHosts,
	"PornHubUser":           pornhubHosts,
	"RCS":                   {"corriere.it", "gazzetta.it"},
	"RTVCPlay":              {"rtvcplay.co"},
	"RTVEALaCarta":          {"rtve.es"},
	"RaiCultura":            {"raicultura.it"},
	"RaiNews":               {"rainews.it"},
	"RedBull":               {"redbull.com"},
	"RokfinChannel":         {"rokfin.com"},
	"Rumble":                {"rumble.com"},
	// SharePoint claims only :v: video views and stream.aspx endpoints;
	// every other sharepoint.com URL is a document or portal page.
	"SharePoint":            {"sharepoint.com/:v:/", "sharepoint.com/stream.aspx"},
	"SimplecastEpisode":     {"simplecast.com"},
	"SimplecastPodcast":     {"simplecast.com"},
	"Sohu":                  {"tv.sohu.com"},
	"Soundcloud":            {"soundcloud.com"},
	"SovietsClosetPlaylist": {"sovietscloset.com"},
	"TV2Article":            {"tv2.no"},
	"TV2Hu":                 {"tv2play.hu"},
	"TV8ItPlaylist":         {"tv8.it"},
	"TVCArticle":            {"tvc.ru"},
	"TVN24":                 {"tvn24.pl", "tvn24bis.pl"},
	"TVP":                   {"tvp.pl", "tvp.info", "tvpparlament.pl", "tvpparlament.info", "tvpworld.com", "swipeto.pl"},
	"TVPVODVideo":           {"tvp.pl"},
	"TarangPlusVideo":       {"tarangplus.in"},
	"ThisOldHouse":          {"thisoldhouse.com"},
	"ToypicsUser":           {"toypics.net"},
	// UnicodeBOM's pattern is a leading-\ufeff match from the commonmistakes
	// module — no hostname can carry it, so no host suffix can route it. The
	// entry exists because the coverage test requires one per residual name;
	// it can never match.
	"UnicodeBOM": {"\ufeff"},
	// VKUserVideos claims only vk.com/video/ paths upstream; the rest of the
	// social network — profiles, photos, docs — stays on the plain lane.
	"VKUserVideos":  {"vk.com/video/", "vkvideo.ru"},
	"Vevo":          {"vevo.com"},
	"Vimeo":         {"vimeo.com"},
	"VimeoGroups":   {"vimeo.com"},
	"VimeoUser":     {"vimeo.com"},
	"WDR":           {"wdr.de"},
	"YouPornVideos": {"youporn.com"},
	"Youtube":       youtubeHosts,
	"YoutubeTab":    youtubeHosts,
	// YoutubeWebArchive claims only Wayback captures of YouTube URLs and the
	// fake host upstream's tests use — a whole-host claim on web.archive.org
	// would route every archived page download to the media lane. The
	// ytarchive: pseudo-scheme in the same pattern has no host and cannot
	// be routed here.
	"YoutubeWebArchive": {"web.archive.org/youtube.com/", "wayback-fakeurl.archive.org/yt/"},
	"ZenYandexChannel":  {"zen.yandex.ru", "dzen.ru"},
}

// youtubeHosts are the first-party YouTube properties the residual Youtube
// patterns claim; the long tail of third-party mirrors in _VALID_URL (dead
// Invidious and Piped instances) is deliberately untracked.
var youtubeHosts = []string{
	"youtube.com",
	"youtu.be",
	"youtube-nocookie.com",
	"youtubekids.com",
	"youtube.googleapis.com",
}

// pornhubHosts is the PornHub alternation's TLD set, premium included.
var pornhubHosts = []string{
	"pornhub.com",
	"pornhub.net",
	"pornhub.org",
	"pornhubpremium.com",
	"pornhubpremium.net",
	"pornhubpremium.org",
}

// knownDRMHosts are the services the KnownDRM extractor claims so yt-dlp can
// answer with its DRM notice instead of aria2 downloading a portal page.
// Entries stay scoped to what the upstream pattern claims — the video
// subdomain or path, never the whole host: amazon's store TLDs route only
// under /gp/video and the music. subdomain, so a store URL stays a plain
// download. plus.rtl.de keeps a whole-host claim because upstream's
// /podcast/ exclusion is a lookahead this grammar cannot express; the
// over-claim is benign — yt-dlp still arbitrates. web.nhk mirrors the
// pattern's literal www.web.nhk host (a real name under the .nhk gTLD).
var knownDRMHosts = []string{
	"play.hbomax.com",
	"channel4.com",
	"channel5.com",
	"peacocktv.com",
	"disneyplus.com",
	"open.spotify.com",
	"tvnz.co.nz",
	"oneplus.ch",
	"artstation.com/learning/courses",
	"philo.com",
	"mech-plus.com",
	"aha.video",
	"mubi.com",
	"vootkids.com",
	"nowtv.it/watch",
	"tv.apple.com",
	"primevideo.com",
	"hulu.com",
	"resource.inkryptvideos.com",
	"joyn.de",
	"amazon.com/gp/video",
	"music.amazon.com",
	"amazon.co.uk/gp/video",
	"music.amazon.co.uk",
	"amazon.de/gp/video",
	"music.amazon.de",
	"amazon.fr/gp/video",
	"music.amazon.fr",
	"amazon.it/gp/video",
	"music.amazon.it",
	"amazon.es/gp/video",
	"music.amazon.es",
	"amazon.ca/gp/video",
	"music.amazon.ca",
	"amazon.co.jp/gp/video",
	"music.amazon.co.jp",
	"amazon.com.au/gp/video",
	"music.amazon.com.au",
	"amazon.in/gp/video",
	"music.amazon.in",
	"watch.njpwworld.com",
	"front.njpwworld.com",
	"qub.ca/vrai",
	"crunchyroll.com",
	"viki.com",
	"deezer.com",
	"b-ch.com",
	"ctv.ca",
	"noovo.ca",
	"tsn.ca",
	"paramountplus.com",
	"crackle.com",
	"sonycrackle.com",
	"cwtv.com",
	"cwtvpr.com",
	"cwseed.com",
	"6play.fr",
	"rtlplay.be",
	"play.rtl.hr",
	"rtlmost.hu",
	"plus.rtl.de",
	"mediasetinfinity.es",
	"tv5mondeplus.com",
	"tv.rakuten.co.jp",
	"watch.telusoriginals.com",
	"video.unext.jp",
	"web.nhk",
	"fod.fujitv.co.jp",
	"zee5.com",
}
