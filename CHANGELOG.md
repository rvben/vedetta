# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).












































## [0.7.14](https://github.com/rvben/vedetta/compare/v0.7.13...v0.7.14) - 2026-07-09

### Fixed

- **camera**: base zone presence on detections, not on coasting tracks ([d70a09b](https://github.com/rvben/vedetta/commit/d70a09b08dcca25cbd45fb3318a55d0cda0a6633))

## [0.7.13](https://github.com/rvben/vedetta/compare/v0.7.12...v0.7.13) - 2026-06-27

### Added

- **media**: default decode to software; hardware decode is opt-in ([da10bf3](https://github.com/rvben/vedetta/commit/da10bf3573a108fbb91a188f530ee9e073f10fc8))
- **media**: build NVDEC without CUDA toolkit and verify it in CI ([896901e](https://github.com/rvben/vedetta/commit/896901ec77d6a1d7692df45ba0d66ca3271e70da))
- **media**: add opt-in Linux VA-API/NVDEC hardware decode ([cf088a0](https://github.com/rvben/vedetta/commit/cf088a080cf35c6bcfb21537abd745c90d2d927c))
- **media**: add VideoToolbox hardware H.264 decode with software fallback ([254624c](https://github.com/rvben/vedetta/commit/254624c80fe3a7279c13616a94b371395610c4e2))

### Fixed

- **media**: build NVDEC without the CUDA toolkit headers ([c5824d2](https://github.com/rvben/vedetta/commit/c5824d28e0fc8b83beed88db65eed30a55a7840d))

## [0.7.12](https://github.com/rvben/vedetta/compare/v0.7.11...v0.7.12) - 2026-06-15

### Added

- **onvif**: renew PullPoint subscription before expiry to avoid event gaps ([80967a2](https://github.com/rvben/vedetta/commit/80967a28c993c39027ce21721db0168fb3baf2de))
- **metrics**: export doorbell press and unanswered metrics ([9f8cebe](https://github.com/rvben/vedetta/commit/9f8cebe8855071d3e930bc4e7cad358e0e0af14c))
- **mqtt**: publish Home Assistant doorbell discovery with lifecycle cleanup ([c7ae275](https://github.com/rvben/vedetta/commit/c7ae275ef0e179e9cb7dcc24c208c16cf317c5f6))
- **camera**: fire the configured doorbell webhook on each press ([9d3af02](https://github.com/rvben/vedetta/commit/9d3af028c2ee83d2ea57b346280774699c52e3d4))
- **notify**: distinct push copy for doorbell rings ([3c4fde0](https://github.com/rvben/vedetta/commit/3c4fde0c62e33fa963354756d134ca4a8fe0448e))
- **ui**: add doorbell ring history page ([9be7051](https://github.com/rvben/vedetta/commit/9be70516e5d0f9ecdf56563793727d5e8ed6d1c0))
- **ui**: persistent doorbell banner with acknowledge/view actions and chime ([29a9cff](https://github.com/rvben/vedetta/commit/29a9cff23104742cde6d5bf621ca71a8cfdaf233))
- **ui**: show answered/missed badge for doorbell rings in the gallery ([ce70ad5](https://github.com/rvben/vedetta/commit/ce70ad5c3546c8bc5b42d0a8a1bbfbf36952bd21))
- **api**: add kind filter to events list and gallery partial ([393522d](https://github.com/rvben/vedetta/commit/393522d95cbe16b7934c02a75db3ef755d5610d4))
- **api**: add idempotent doorbell answer endpoint with SSE broadcast ([bc8d74e](https://github.com/rvben/vedetta/commit/bc8d74ed172761f176d806fd2fcd927dda5156cc))
- **onvif**: debounce doorbell presses and inject them into the event loop ([a6e593a](https://github.com/rvben/vedetta/commit/a6e593a2136ca914f40ba4d9064fec0f2c2a7da9))
- **events**: route doorbell rings through the loop with synthetic-end clips ([0e5da5a](https://github.com/rvben/vedetta/commit/0e5da5afbf2575684e0618b53f7535c0c25476d5))
- **camera**: add SubmitDoorbellPress event injection primitive ([00b258f](https://github.com/rvben/vedetta/commit/00b258fa687ce1cdcfc8c5481c7cacc1d396d7e6))
- **config**: add global doorbell defaults with per-camera overrides ([fad4136](https://github.com/rvben/vedetta/commit/fad41368d059b11d17c64ec5b0f71d5947c66dd2))
- **storage**: add doorbell kind + answer columns and queries ([07f7a2a](https://github.com/rvben/vedetta/commit/07f7a2a021cd4d92f214c22fee49da4bd487c131))
- **camera**: add Kind and answer fields to Event ([4eebbac](https://github.com/rvben/vedetta/commit/4eebbac164153b5d9b127044af3158ee85dc6e40))

### Fixed

- **onvif**: parse PullMessages NotificationMessage at the correct nesting ([cc051b1](https://github.com/rvben/vedetta/commit/cc051b1afd9e77c3def337ee7d012a30dfcf2898))
- **onvif**: discover event service port, use WS-Security and WS-Addressing for subscriptions ([0cc675f](https://github.com/rvben/vedetta/commit/0cc675f9ce01af0d14aadaa1059f064e36ba8b9d))
- **ui**: stop doorbell answered/missed badge overlapping the score badge ([6a23054](https://github.com/rvben/vedetta/commit/6a23054e2220a474b9938850608ec8bed5609658))
- **doorbell**: kind-filter event counts and guard answer to doorbell rings ([76cbaec](https://github.com/rvben/vedetta/commit/76cbaeca5331f5d0e38c6a85f22f298ca6e954a2))
- **doorbell**: publish trigger from emit path, skip object-count, enrich banner with recognized person ([2eec91f](https://github.com/rvben/vedetta/commit/2eec91fe5007465006f465075945b4e6fafc9463))

## [0.7.11](https://github.com/rvben/vedetta/compare/v0.7.10...v0.7.11) - 2026-06-14

### Added

- **api**: warm running cameras' live-HLS consumers on a reconcile loop ([6c25596](https://github.com/rvben/vedetta/commit/6c25596d5a2d5e068454300e6718296acb3a986b))
- **camera**: add RunningCameraDetectURLs for HLS warming ([5d8a03f](https://github.com/rvben/vedetta/commit/5d8a03f2eeadaa99e87d11a2a7b7dea0c3fedbde))
- **stream**: add SetWarmURLs to keep live-HLS consumers warm ([38a4234](https://github.com/rvben/vedetta/commit/38a4234c868ed26261a1293c56c42ef6e24eca20))
- **stream**: add warm-consumer set and manager lifetime to HLSManager ([c21f7a3](https://github.com/rvben/vedetta/commit/c21f7a3e7b5f3cc0da31ee06f8e5bb16aa48b929))
- **stream**: make HLS consumer creation rebuild on source recreation ([49a07c4](https://github.com/rvben/vedetta/commit/49a07c43e30cb2675417643324f4843e3496b9f4))
- **watchdog**: graceful self-restart on sustained memory pressure ([069e9ae](https://github.com/rvben/vedetta/commit/069e9aef6a09cdaf65121b5a2e1867a63c6e0051))

### Fixed

- **detect**: stop concurrent inference latching GC off ([4fc9866](https://github.com/rvben/vedetta/commit/4fc9866db823f8633b9200eb74b6dccef5a1152e))
- **stream**: anchor playback HLS to wall-clock time across file boundaries ([6f0c5c3](https://github.com/rvben/vedetta/commit/6f0c5c38b58778e5bb82f92b33e6c52d59508f8c))
- **api**: resolve recording day boundaries in the viewer's timezone ([5e139f8](https://github.com/rvben/vedetta/commit/5e139f82f8cd21ff2dfeba3e53ff8db576776780))
- **recording**: rotate segment on video RTP timestamp gap ([056d156](https://github.com/rvben/vedetta/commit/056d156e941a0fcde85366a71d4d42c703f52307))
- **ui**: cap timeline seek snap tolerance at 15 seconds ([934ae9b](https://github.com/rvben/vedetta/commit/934ae9b0bde5475ab1d7b4d585853705b49aad38))
- **camera**: defer event-snapshot copy until a track emits an event ([4ab4a94](https://github.com/rvben/vedetta/commit/4ab4a94ac5ba35129467695dd2e3663257a69e4a))
- **stream**: clamp over-declared H.264 level so iOS HLS starts fast ([0761d7b](https://github.com/rvben/vedetta/commit/0761d7b577deadb7c7b403c46883420f3bd179c0))
- **stream**: strip SEI NALs from live HLS so iOS decodes proprietary-SEI cameras ([e0b7ad2](https://github.com/rvben/vedetta/commit/e0b7ad2c32e81461660188b4aa70a7e13fd6391c))
- **api**: guard nil context in warm HLS reconcile loop ([1597aa9](https://github.com/rvben/vedetta/commit/1597aa9dce2a73224d880f8b284d05ec98ddd01f))
- **stream**: rebuild warm HLS consumer poisoned before video params ([72f042d](https://github.com/rvben/vedetta/commit/72f042d3fc9f828f87ba89e4a388ad2a666385e5))
- **recording**: delete orphaned segment rows when the file is missing ([9f5fb68](https://github.com/rvben/vedetta/commit/9f5fb68bf2df42c40fd49bdec12bfc60fef40be0))
- **recording**: run clip recompression in an isolated subprocess ([526290a](https://github.com/rvben/vedetta/commit/526290ab671818a77185971e2af30a2b10b20ec7))

## [0.7.10](https://github.com/rvben/vedetta/compare/v0.7.9...v0.7.10) - 2026-06-02

### Added

- tier events as alert or detection so parked vehicles are low priority ([19a9d64](https://github.com/rvben/vedetta/commit/19a9d64be081a581a8efbcf29894d0bd7b94c728))

### Fixed

- address review findings on event tiering and re-ID ([0cccb68](https://github.com/rvben/vedetta/commit/0cccb68a55e36317b9953d2d0b8e7489fa2f98a8))
- **detect**: assign only the best-matching known object and raise re-ID threshold ([a43df09](https://github.com/rvben/vedetta/commit/a43df09127ab1f67b0266a669d2af772976216a8))

## [0.7.9](https://github.com/rvben/vedetta/compare/v0.7.8...v0.7.9) - 2026-06-02

### Added

- **rtsp**: log the negotiated transport on connect ([94ee3ea](https://github.com/rvben/vedetta/commit/94ee3eaf4a08f6925f77e4c55d30a1b88d4f04ce))
- **camera**: re-confirm stationary objects to keep parked cars as one event ([e09f1d1](https://github.com/rvben/vedetta/commit/e09f1d1b72c8c7e8e79c2416381d029011f778aa))
- **rtsp**: add per-camera RTSP transport (tcp/udp/auto) ([ebfba56](https://github.com/rvben/vedetta/commit/ebfba56e4d6099f510baba3ba8cd77d1fed2c069))

### Fixed

- **rtsp**: apply per-camera transport regardless of which consumer connects first ([e683b5a](https://github.com/rvben/vedetta/commit/e683b5a467be9db358f6b6edd9102deef7dc5b0f))
- **detect**: keep parked vehicles as one track to stop duplicate events ([ef335d8](https://github.com/rvben/vedetta/commit/ef335d87c8fdf16d80e12db778d202d55b6ee3cf))

## [0.7.8](https://github.com/rvben/vedetta/compare/v0.7.7...v0.7.8) - 2026-06-02

### Fixed

- **web**: use the raw camera name for dashboard snapshot refresh ([4c5c1d0](https://github.com/rvben/vedetta/commit/4c5c1d049f8d370d94c53a0a36a3f2d484a57f6f))

## [0.7.7](https://github.com/rvben/vedetta/compare/v0.7.6...v0.7.7) - 2026-06-02

### Fixed

- **rtsp**: resolve data race between probe timeout Close and client Start ([be58400](https://github.com/rvben/vedetta/commit/be58400d901f2b91bc2fb9476b8e7b7ff393620f))

## [0.7.6](https://github.com/rvben/vedetta/compare/v0.7.5...v0.7.6) - 2026-06-02

### Added

- **api**: expose camera last-seen and serve stale snapshots when offline ([9a82689](https://github.com/rvben/vedetta/commit/9a82689369d457ccc99e47d5a5dbecfaa569c94e))
- harden against runtime wedges with a supervisor and log rotation ([4b4662b](https://github.com/rvben/vedetta/commit/4b4662ba45cb922230df96f5e4de285d8877f9e2))

### Fixed

- **stream**: stop webrtc from mutating the shared fan-out packet ([83d5ea5](https://github.com/rvben/vedetta/commit/83d5ea5537822f72eeed1e7958deb3e6b1ab7d46))
- **media**: prevent use-after-free of the OpenH264 decoder on teardown ([3dd8972](https://github.com/rvben/vedetta/commit/3dd897201dad2aeacd2948bee4f14153c8441799))

## [0.7.5](https://github.com/rvben/vedetta/compare/v0.7.4...v0.7.5) - 2026-05-30

### Added

- **ui**: play recording segments inline on the recordings page ([b764246](https://github.com/rvben/vedetta/commit/b76424654243302b0cf52fe8840f24dfa90af1b0))
- **ui**: add keyboard hint title to timeline track ([efab281](https://github.com/rvben/vedetta/commit/efab28184f7c0fd289c1fef18cd2db336a0a157b))
- **ui**: humanize camera name in breadcrumb and title ([16fcbf6](https://github.com/rvben/vedetta/commit/16fcbf6a3deba6eb476403d2911b778d15553ab9))
- **ui**: set initial timeline slider aria value on load ([aa1714f](https://github.com/rvben/vedetta/commit/aa1714f832b9866a48a245e49fe4e27eb71db2ba))
- **ui**: add zoom in/out buttons to the timeline ([7edc8c0](https://github.com/rvben/vedetta/commit/7edc8c0d6525fc967ddae0da766071645671ed50))
- **ui**: add discoverability hint for timeline zoom/pan ([8be6b73](https://github.com/rvben/vedetta/commit/8be6b737d64bbbc18da7bb078ff7190e54a90d9a))
- **ui**: improve setup wizard accessibility and add back navigation ([fb528c0](https://github.com/rvben/vedetta/commit/fb528c0ff5b7fa7138920c560c51a85031400193))
- **ui**: improve login accessibility (live errors, password toggle, focus, copy) ([d979792](https://github.com/rvben/vedetta/commit/d9797925a93bc1cd3c1280df1aed4ea443550328))
- **ui**: event detail UX improvements - label, a11y, layout, styles ([5ec8fa6](https://github.com/rvben/vedetta/commit/5ec8fa637f3d960aab7ea802e605464411fc2b0e))
- **ui**: title-case detection label in document title ([c299f65](https://github.com/rvben/vedetta/commit/c299f6543efea1cf4bdf2e843e515616a7309cf8))
- **ui**: show snapshot poster while event clip buffers ([59da90c](https://github.com/rvben/vedetta/commit/59da90cea717c726abdf0aafdafbce12b2f11dfb))
- **ui**: expose dashboard view-toggle state via aria-pressed ([31e4981](https://github.com/rvben/vedetta/commit/31e4981617c4fd9a32c5e79421635c86d7754319))
- **ui**: humanize camera names and labels on dashboard cards ([3bda680](https://github.com/rvben/vedetta/commit/3bda680025f959dc5e48b87bfc6b3476d12aa015))
- **ui**: add page title and skip-to-content link to dashboard ([e4539e6](https://github.com/rvben/vedetta/commit/e4539e635d775d4e68134f6ff2c311c37f3023d6))
- **ui**: add empty and loading states to recordings ([3157418](https://github.com/rvben/vedetta/commit/3157418bdbae004850d6d9ccb6eaa26c0b7faca1))
- **ui**: show total coverage duration on recordings segment toggle ([915acf9](https://github.com/rvben/vedetta/commit/915acf96207b84cb59405f03e6b04545f8d919bc))
- **ui**: explain recording gaps on the coverage timeline ([6bb1db4](https://github.com/rvben/vedetta/commit/6bb1db46f8bcd4836f4a25c9470da5942967207d))
- **ui**: add accessible names and roles to recordings controls ([b2a3bea](https://github.com/rvben/vedetta/commit/b2a3beafcd5d69585f9ea9338e755a9774a1a7ca))
- **ui**: flag duplicate people, show last-seen, handle empty thumbnails ([c89d842](https://github.com/rvben/vedetta/commit/c89d8428b2f423b3471ba3f5fa0e4b59ddf57b03))
- **ui**: disambiguate duplicate objects and label confirm modal ([7862209](https://github.com/rvben/vedetta/commit/7862209263dd6f4c5031fce9ac7e0fa44cbca81d))
- **ui**: make object/person thumbnails keyboard-accessible ([e84398d](https://github.com/rvben/vedetta/commit/e84398d77980a3efe4eed0cd5939b2bb9d9b0cbd))
- **ui**: add accessible names to editable name fields ([bc83b23](https://github.com/rvben/vedetta/commit/bc83b2317bbaf109ab6559c7e6c970b33976e799))
- **ui**: make editable name fields visibly editable ([a886b82](https://github.com/rvben/vedetta/commit/a886b82c8bc59b437d2fcbed586f5eb60da88767))
- **ui**: add unit hints and busy state to settings controls ([99cdeb4](https://github.com/rvben/vedetta/commit/99cdeb4403aeeece4396d0cf3e68553e9c30669a))
- **ui**: announce settings save status via aria-live ([626b0ef](https://github.com/rvben/vedetta/commit/626b0efad1260e1d03c718f39c02be9bb83662d0))
- **ui**: associate labels with settings form controls ([5bf170c](https://github.com/rvben/vedetta/commit/5bf170c6499bfc1ade150f2ea0141684f9f10d44))
- **ui**: add relative oldest-time and loading state to storage ([cc1a42b](https://github.com/rvben/vedetta/commit/cc1a42b87a9dd00951145a508630cbf83bba7fd4))
- **ui**: show visible tooltips on storage usage spark bars ([eef4c51](https://github.com/rvben/vedetta/commit/eef4c51e38ee6eb587c5e825ca4765001f7403ef))
- **ui**: label storage action buttons with camera context ([27f0e0d](https://github.com/rvben/vedetta/commit/27f0e0d1c2eb479af369f0bdcc64142c93804a8e))
- **ui**: add disk capacity gauge and total to storage page ([7dacfc2](https://github.com/rvben/vedetta/commit/7dacfc2c7bbfd4e9a4d8685921ad22d09ec0a58e))
- **ui**: add loading skeletons and end-of-list signal to events ([2285748](https://github.com/rvben/vedetta/commit/228574825fa7691f8666689fb529b28368a46509))
- **ui**: color event confidence badge by score ([2d0f2ec](https://github.com/rvben/vedetta/commit/2d0f2ecec0d8b82d5325fb460423bb2a10a1445f))
- **ui**: expose filter chip selected state via aria-pressed ([1b22b48](https://github.com/rvben/vedetta/commit/1b22b48fbf19502d9e677c6f7a013717f816836c))
- **ui**: humanize camera names and event thumbnail alt text ([94fae87](https://github.com/rvben/vedetta/commit/94fae87b2b13e6c80dccade3a0f27c506c8c5555))
- **api**: add displayName template helper for humanizing slugs ([d3b0b7f](https://github.com/rvben/vedetta/commit/d3b0b7f447a71e1ed8d33b07488318640d511e1e))
- **ui**: ensure visible focus ring on all focusable controls ([c514f70](https://github.com/rvben/vedetta/commit/c514f701ac1b02e70bbeb3a623cd56646f88b329))
- **ui**: reveal hover-only controls on touch devices ([1419d21](https://github.com/rvben/vedetta/commit/1419d21e77705d82c175fb58e65a27c6c37303a2))
- **ui**: enlarge small controls to 44px on touch devices ([3c687a7](https://github.com/rvben/vedetta/commit/3c687a746c5044413271de7a85e73895cf752f00))
- **ui**: minimap tap-to-recenter and drag-to-pan ([eae94cd](https://github.com/rvben/vedetta/commit/eae94cd76108cff5718ab04b6a3dd14a7d472881))
- **ui**: keyboard control and aria values for timeline slider ([1b83db2](https://github.com/rvben/vedetta/commit/1b83db2dedf9096444fb4e2bc3b00a3850e0a8ca))
- **ui**: viewport-aware timeline defaults + live-follow + LIVE wiring ([9e89184](https://github.com/rvben/vedetta/commit/9e89184faecc4693e92c56520eaf575f4641a524))
- **ui**: pointer-event zoom/pan/tap gestures on timeline track ([2d98d5a](https://github.com/rvben/vedetta/commit/2d98d5a8712a6ea0440c6a07f8e48dc9a15d36b4))
- **ui**: add timeline minimap and dynamic axis labels ([f2e30dd](https://github.com/rvben/vedetta/commit/f2e30dd29fdc3c3e9541c5cc627772cff33c8297))
- **ui**: add timeline-window event, coverage, and seek helpers ([979b40c](https://github.com/rvben/vedetta/commit/979b40c0663dc7001b52b413003caf4ddf88c941))
- **ui**: add timeline-window tick + snap-tolerance helpers ([68cab72](https://github.com/rvben/vedetta/commit/68cab722dfc97c196dbc6c9f836c5fd730c346bc))
- **ui**: add timeline-window default/viewport/reset helpers ([ad0917a](https://github.com/rvben/vedetta/commit/ad0917a9381f7e781376f6f6d1829fdb1438d500))
- **ui**: add timeline-window zoom/pan/follow helpers ([a351b2a](https://github.com/rvben/vedetta/commit/a351b2a06b00ce9cd6880012565deb3209bac3ca))
- **ui**: add timeline-window math module (core mapping) ([54a148d](https://github.com/rvben/vedetta/commit/54a148d180ca65d80a3972a0c2b5e9eb1d0e5ef7))

### Fixed

- **ui**: use hyphens instead of en/em dashes in recordings and people labels ([d9ce148](https://github.com/rvben/vedetta/commit/d9ce148c5e20b6ef62f31e7098a3184b80493f82))
- **ui**: suppress detection overlay and keep snapshot until live video has a frame ([d05cd3e](https://github.com/rvben/vedetta/commit/d05cd3ed664a755cdefdeab17f82e9158322c9f6))
- **ui**: stack storage per-camera actions on mobile to stop overflow ([67ed307](https://github.com/rvben/vedetta/commit/67ed307170295470b8b55fdacde273bd40ad54ae))
- **ui**: let vertical page scroll pass through the timeline minimap ([8406116](https://github.com/rvben/vedetta/commit/84061164a962bb5513e59594bddbcc408d2613f8))
- **ui**: stop lone camera tile from stretching full width ([a621e5d](https://github.com/rvben/vedetta/commit/a621e5d112ce90613ee55969465a9d4e73a2c1bb))
- **ui**: make recordings segment rows usable on narrow viewports ([abcfc58](https://github.com/rvben/vedetta/commit/abcfc586be12262930e8ccb8fe5603ff49452201))
- **ui**: wrap page header on mobile and label page sections ([d6f231d](https://github.com/rvben/vedetta/commit/d6f231dac6ded078ad99ab4e131de29eb27dcbea))
- **ui**: align settings mobile breakpoint and prevent horizontal overflow ([fa340ae](https://github.com/rvben/vedetta/commit/fa340aed2548c16ea2d3a34f32584f13d853c0c4))
- **ui**: make storage per-camera table usable on narrow viewports ([3bf147e](https://github.com/rvben/vedetta/commit/3bf147e15e6919683631e104cf4174ab9d3f6cda))
- **ui**: wrap events legend on narrow viewports ([1f4e7ab](https://github.com/rvben/vedetta/commit/1f4e7ab0c74e2d5e4cd1d7865184f879f325c15c))
- **ui**: show human-readable durations in settings ([68b4494](https://github.com/rvben/vedetta/commit/68b449463b1918e4517921e8622eabce88bf86fa))
- **ui**: make event-detail breadcrumb time match the metadata card ([6f1c8e5](https://github.com/rvben/vedetta/commit/6f1c8e5fdafff04aa9c0fe852740390f0ba30646))
- **ui**: use defined tokens for setup config-readonly error styling ([eb26417](https://github.com/rvben/vedetta/commit/eb26417cef41cb211f8dca39b20ecdc7787a6a1c))
- **ui**: keep object filter active during events infinite scroll ([cc03758](https://github.com/rvben/vedetta/commit/cc03758732dd4c765df991e83dc1c21fd604bf48))
- **ui**: move event duration badge to top-left to stop overlap with label ([db0c611](https://github.com/rvben/vedetta/commit/db0c611bb04ae07eb628c96fc177aec2ea328be6))
- **ui**: raise tertiary-text contrast to meet WCAG AA ([d9db85e](https://github.com/rvben/vedetta/commit/d9db85e71bcdb6f50692a376e380b6573ae9e37e))
- **ui**: correct pan-start jump, playback-during-pan, and multi-pointer edge cases ([5b0925a](https://github.com/rvben/vedetta/commit/5b0925a776ca8aa2626f85ead45a678aa9fa4a44))
- **ui**: hide timeline playhead on past dates each frame; simplify live-follow gate ([ab4a63b](https://github.com/rvben/vedetta/commit/ab4a63b025b37fc6dd9687eca96deb40f47e9e80))
- **ui**: align live snapshot backdrop sizing with foreground transports ([f881b7c](https://github.com/rvben/vedetta/commit/f881b7c27a9121a2aa7e41fc5042557f416e78ee))

### Performance

- **ui**: lazy-load object/person thumbnails ([f6c115d](https://github.com/rvben/vedetta/commit/f6c115d355cf29bcf9c05687586d40e8c16d0f47))
- **ui**: load settings sections concurrently ([09f68ad](https://github.com/rvben/vedetta/commit/09f68ad079a661929ce11e1f87261271038e999c))

## [0.7.4](https://github.com/rvben/vedetta/compare/v0.7.3...v0.7.4) - 2026-05-28

### Added

- **api**: track active HLS viewers per camera ([99560a0](https://github.com/rvben/vedetta/commit/99560a0e9b0dfffc2409941bdd7ff190210744ed))
- **api**: annotate /metrics with HELP/TYPE, rename event/segment gauges, expose stream client counts ([f738cdd](https://github.com/rvben/vedetta/commit/f738cdd6c4a53a8fe7fe7247fe1c223a24e253e9))
- **api**: track active MJPEG viewers per camera ([dcb0ef2](https://github.com/rvben/vedetta/commit/dcb0ef22f45882d12bd9102af19d8fd75a2618dd))
- **stream**: expose per-camera WebRTC peer counts ([26bd5b4](https://github.com/rvben/vedetta/commit/26bd5b46456761ee42699c3a154505f6ff83a3b1))
- **stream**: expose per-camera MSE client counts ([576619c](https://github.com/rvben/vedetta/commit/576619cdce797c9d5c7ee1bd149f008992bf6b8e))
- **cli**: add 'auth create-token' to mint scoped API tokens offline ([080c17c](https://github.com/rvben/vedetta/commit/080c17cf25908cb3527bc66204852a1d934cfa8f))
- **metrics**: expose Go runtime, GC, and process-RSS metrics ([ee0587d](https://github.com/rvben/vedetta/commit/ee0587dda4f11a339ab25ab8c96811de35fc2660))
- **metrics**: add platform process-RSS reader ([96fcbd6](https://github.com/rvben/vedetta/commit/96fcbd6b8cd11fb6fea85024dcc730a64b06720d))
- **storage**: surface recompression controls on the storage page ([d7a7180](https://github.com/rvben/vedetta/commit/d7a7180fcc99bb4d11ec98eb025c531147c1adbc))

### Fixed

- **storage**: keep recompression state out of the 30s breakdown cache ([bbcbc3a](https://github.com/rvben/vedetta/commit/bbcbc3ad9d81d3e89e3d572d40ac979bae90a144))

## [0.7.3](https://github.com/rvben/vedetta/compare/v0.7.2...v0.7.3) - 2026-05-27

### Added

- **api**: surface clips_recompressed in health and system partial ([80c3a2c](https://github.com/rvben/vedetta/commit/80c3a2c9da954b144e6c012de3df6e5d0df612d0))
- **recording**: expose clips_recompressed in recompression stats ([931d9e0](https://github.com/rvben/vedetta/commit/931d9e09223830c929444f821ad29d407a4dae94))
- **recording**: label recompression kind in logs ([c108133](https://github.com/rvben/vedetta/commit/c10813387cf4402105cc5c7b13437080cdb85364))
- **recording**: recompress event clips alongside segments ([6f4f1e5](https://github.com/rvben/vedetta/commit/6f4f1e58c91e64be70efb0de144119c8822bf8e7))
- **storage**: backfill legacy clip sizes and reconcile missing files ([04e4bd5](https://github.com/rvben/vedetta/commit/04e4bd5d20e3d3fca28ff95249a442b821b10672))
- **storage**: add GetClipRecompressState revalidation read ([2c4d04e](https://github.com/rvben/vedetta/commit/2c4d04ed71cf629fcf1a31e59bac8619bea49f6d))
- **storage**: add clip recompress mark/increment/reset-stuck methods ([c6be6ab](https://github.com/rvben/vedetta/commit/c6be6ab32a05c3e72da13e10d7eeb48589b9a6f2))
- **storage**: add clip recompression candidate queries ([7590757](https://github.com/rvben/vedetta/commit/7590757e59a87d322d184464207879bef373724a))
- **storage**: add SetEventClip/ClearEventClip resetting clip recompression state ([bd47611](https://github.com/rvben/vedetta/commit/bd47611e63307bcac83cfc3a93136f1c2389530c))
- **storage**: add events clip-recompression columns and v3 migration ([44370a1](https://github.com/rvben/vedetta/commit/44370a1fb94b8cc618fb778792ab1d6ff6456029))

### Fixed

- **recording**: preserve event clip mtime across recompression so retention is unaffected ([d1c2872](https://github.com/rvben/vedetta/commit/d1c287264f48e53080b405eb23a15f444e0e7abd))
- **recording**: skip to next-largest segment when largest is HLS-served ([96b0cab](https://github.com/rvben/vedetta/commit/96b0cab8a2f55e2ebac00f1d1736144aa719c9e0))

## [0.7.2](https://github.com/rvben/vedetta/compare/v0.7.1...v0.7.2) - 2026-05-27

### Added

- **config**: reject unknown keys with strict YAML decoding ([80e995c](https://github.com/rvben/vedetta/commit/80e995c64a961824a2b476d60ac66ab66f270b31))
- **tracing**: support OTLP export headers in config ([c11aa33](https://github.com/rvben/vedetta/commit/c11aa33a675b72b97d34e4ae75e6d0dccede40ba))

## [0.7.1](https://github.com/rvben/vedetta/compare/v0.7.0...v0.7.1) - 2026-05-27

### Added

- **logging**: support custom OTLP headers for log export ([9a8a478](https://github.com/rvben/vedetta/commit/9a8a4789dba06df7b5a3088179ff586bef7628a3))

## [0.7.0](https://github.com/rvben/vedetta/compare/v0.6.2...v0.7.0) - 2026-05-27

### Added

- **logging**: wire OTLP log export into startup ([7015822](https://github.com/rvben/vedetta/commit/70158222a5202f667dc0078ab5e37e6b8b39fe93))
- **config**: add logging OTLP export config block ([9f38842](https://github.com/rvben/vedetta/commit/9f38842ce6f6915f66fc7a81d444b2f3a8ece097))
- **logging**: add OTLP log exporter Init and Provider ([625c3d2](https://github.com/rvben/vedetta/commit/625c3d290f126b560ccd358b479983f1c8ae1ba5))
- **logging**: add logs OTLP endpoint/protocol resolution ([efe1f3a](https://github.com/rvben/vedetta/commit/efe1f3a43a8732c09b4ef215e466b4fcaed27884))
- **logging**: add fan-out slog handler ([af6e42b](https://github.com/rvben/vedetta/commit/af6e42bcd644aeec9b62914743df5967d28cfe8e))
- **logging**: add level-gate slog handler ([6f13d4e](https://github.com/rvben/vedetta/commit/6f13d4e8e5542e803f9ba2fa1816fda79454885f))
- **otelexport**: add shared OTLP endpoint classification ([411c1f4](https://github.com/rvben/vedetta/commit/411c1f4487510b63d11e811140c6e736675c1d68))
- **auth**: add least-privilege metrics:read scope for /metrics scraping ([243e4c5](https://github.com/rvben/vedetta/commit/243e4c5dea9f07dfa753788a0eee8ab0a512bb4c))

### Fixed

- **config**: keep logging.protocol unset so transport fallback works ([74fdef1](https://github.com/rvben/vedetta/commit/74fdef1b6b9ade56aa2e3b6517eee2711032b333))
- **config**: normalize tracing and logging protocol before validation ([da1ae0c](https://github.com/rvben/vedetta/commit/da1ae0c5c548890489762fec416ee30c13090cb2))
- **config**: validate logging.protocol like tracing ([09b2c2e](https://github.com/rvben/vedetta/commit/09b2c2e45cb91e51343e5b6acc93733b1479c7ff))
- **logging,tracing**: share rate-limited OTLP error handler across signals ([124fe71](https://github.com/rvben/vedetta/commit/124fe71450e35c04631e58634bc88b9ed4112c05))
- **logging**: tracing fallback protocol beats generic env for atomicity ([2937850](https://github.com/rvben/vedetta/commit/293785054090a112a47642ff65335fa9e14a836c))
- **logging**: reuse tracing transport atomically on endpoint fallback ([34c5945](https://github.com/rvben/vedetta/commit/34c5945576e433bba20102744011b5e7e89d37e0))
- **logging**: deep-clone grouped attrs per arm in fanout WithAttrs ([ca0c0be](https://github.com/rvben/vedetta/commit/ca0c0be8a22a640b8a5a6d55f9ee6d2975e5b1b7))
- **logging**: clone attrs per arm in fanout WithAttrs ([48cdc31](https://github.com/rvben/vedetta/commit/48cdc3105fd89a750ab4a566a73fb15e886a430c))
- **logging**: enforce level floor in levelGate.Handle for fan-out use ([01eae28](https://github.com/rvben/vedetta/commit/01eae28c31f1d75429454dc8fe3914b9f658224b))

## [0.6.2](https://github.com/rvben/vedetta/compare/v0.6.1...v0.6.2) - 2026-05-26

### Added

- **metrics**: add HTTP RED metrics (request count + latency by status class) ([e63deb5](https://github.com/rvben/vedetta/commit/e63deb5d666ac41afe3cf66110952120ad3caf6f))

### Fixed

- **mqtt**: propagate object-count and presence publish errors ([588927b](https://github.com/rvben/vedetta/commit/588927bc7ef9d6d48bc50b9e592b9b1f1693a5a7))
- **mqtt**: bound publish waits and span event-loop publishes ([46ed0cf](https://github.com/rvben/vedetta/commit/46ed0cf112820ab0436a34826296c2c9c1e35895))

## [0.6.1](https://github.com/rvben/vedetta/compare/v0.6.0...v0.6.1) - 2026-05-25

## [0.6.0](https://github.com/rvben/vedetta/compare/v0.5.4...v0.6.0) - 2026-05-25

### Added

- **metrics**: add detection-pipeline latency histograms and frame counters ([3cf3000](https://github.com/rvben/vedetta/commit/3cf30005808e0cfc08598710ed94e8f6b6657020))
- **metrics**: add per-camera RTSP reconnect counter ([45b79de](https://github.com/rvben/vedetta/commit/45b79deab32a71b91f8787b3f182c16009ed2dbd))
- **metrics**: count frames dropped to slow detection-SSE and MSE clients ([24c7d18](https://github.com/rvben/vedetta/commit/24c7d1858adb4aa9449a836260ac8fe780cf9961))
- **recording**: instrument clip.extract with attempt and stats ([41affd4](https://github.com/rvben/vedetta/commit/41affd41c485d14701220454fa8419c5bcc61622))

### Fixed

- **api**: guard server lifecycle against Start/Shutdown race ([e229305](https://github.com/rvben/vedetta/commit/e229305dd84673e3940549042928159db9888872))
- **metrics**: snapshot histogram series under a lock for scrape consistency ([f0c826a](https://github.com/rvben/vedetta/commit/f0c826ac7c2251b6399f3260faf2629b27955c02))

## [0.5.4](https://github.com/rvben/vedetta/compare/v0.5.3...v0.5.4) - 2026-05-25

### Added

- **events**: break down mqtt.publish span into encode and broker sub-spans ([17fa8fd](https://github.com/rvben/vedetta/commit/17fa8fd61defda69c3e9d5aa4f1c23ea00048e34))

## [0.5.3](https://github.com/rvben/vedetta/compare/v0.5.2...v0.5.3) - 2026-05-25

### Added

- **events**: add waitForEmit to order event-end after create publish ([efa68ac](https://github.com/rvben/vedetta/commit/efa68ac4e74d731bd1afd62ca35b2e41652b90a1))
- **events**: add emitEventArtifacts for off-loop snapshot/MQTT/push ([002370e](https://github.com/rvben/vedetta/commit/002370e8c539f6e97e0d1edbae04fc3ed4de55c1))

### Performance

- **events**: offload snapshot/MQTT/push from the event loop ([8e55664](https://github.com/rvben/vedetta/commit/8e5566488fd21ce19216a517dafddfdac27f7234))

## [0.5.2](https://github.com/rvben/vedetta/compare/v0.5.1...v0.5.2) - 2026-05-24

### Added

- **tracing**: clip.extract span via testable extractClipSpan helper ([b2575aa](https://github.com/rvben/vedetta/commit/b2575aa110c29538cb2f05d98da0d386d14b7885))
- **tracing**: event.end span parented to the stored event root ([316c94c](https://github.com/rvben/vedetta/commit/316c94ceb0e1d164cd88d5a5cebd2803880bb940))
- **tracing**: object.reid child span on the detached re-ID goroutine ([a3d962b](https://github.com/rvben/vedetta/commit/a3d962b2af5d7d3b4d0cbb35a93d72d31f29aee9))
- **tracing**: event root span with db/snapshot/mqtt children ([c2c9439](https://github.com/rvben/vedetta/commit/c2c94396e2b1678e1b7dfeefb7bd7ba79f7e3ff9))

## [0.5.1](https://github.com/rvben/vedetta/compare/v0.5.0...v0.5.1) - 2026-05-23

### Added

- wire opt-in OpenTelemetry tracing into the server lifecycle ([a4a5b5f](https://github.com/rvben/vedetta/commit/a4a5b5f32b5697ddf935ad740ecb93d10c000f01))
- **api**: otelhttp request spans gated behind tracing config ([c6cacf6](https://github.com/rvben/vedetta/commit/c6cacf65a679f62f5d010734ac6865090323f1eb))
- **tracing**: provider init, no-op fallback, bounded shutdown ([8dea325](https://github.com/rvben/vedetta/commit/8dea325eec16c3c262d698d16bfa14665e84fdd3))
- **tracing**: prefix-keyed ParentBased sampler ([72a0221](https://github.com/rvben/vedetta/commit/72a022195f99f673a3fdb6f3ab30e308ab3838f3))
- **tracing**: OTLP endpoint/env resolution and transport selection ([9c2dbd7](https://github.com/rvben/vedetta/commit/9c2dbd7b3fbfb02d102b2916cab73c2133fe021d))
- **config**: add tracing config block with defaults and validation ([24b5589](https://github.com/rvben/vedetta/commit/24b5589ec0892dc952d03ebccd1b2a67fedce888))

## [0.5.0](https://github.com/rvben/vedetta/compare/v0.4.4...v0.5.0) - 2026-05-23

### Added

- **api**: tighten CSP connect-src and add gated HSTS header ([71b8968](https://github.com/rvben/vedetta/commit/71b896812acec1e935e86f20320bfaead8cde37e))
- **auth**: require admin scope for config-mutation endpoints ([bba544a](https://github.com/rvben/vedetta/commit/bba544a2f76f103197ccf80a0f8910e1d8f46047))
- **webrtc**: make ICE servers configurable, default to none ([f47e8cf](https://github.com/rvben/vedetta/commit/f47e8cf22cccf6a767dce8f08a211f63ea91c97f))

### Fixed

- **api**: gate HSTS on actual transport, not exposure policy ([4c7bfd3](https://github.com/rvben/vedetta/commit/4c7bfd3de569a8095aaa2fde84e8bb3a79e17335))
- **stream**: cancel WebRTC stats logger on HandleOffer error paths ([6260306](https://github.com/rvben/vedetta/commit/62603066b6499d9e38a43c9da5b8d39921b97d17))
- **recording**: honor shutdown context during clip extraction ([3849695](https://github.com/rvben/vedetta/commit/3849695d30798830db19a534d9d78ba359b5d601))
- **recording**: surface DB errors when clearing deleted clip references ([bc5c453](https://github.com/rvben/vedetta/commit/bc5c453bc23e61f2f430dd5e64a8e572577b1ba0))
- **rtsp,camera**: jitter reconnect backoff to avoid thundering herd ([d231d60](https://github.com/rvben/vedetta/commit/d231d60641d7dce36893276431b2a5f404853a0e))
- **storage**: make SavePushSubscription an atomic upsert ([ba3aa34](https://github.com/rvben/vedetta/commit/ba3aa342d4e32ac0b62a12829535855aa197f1f8))
- **media**: make SnapshotConsumer.Close idempotent ([1c59a4f](https://github.com/rvben/vedetta/commit/1c59a4fc31bf7e8963c88dc27600d716783bfc00))
- **storage**: surface swallowed errors in zone lookup and token listing ([029152b](https://github.com/rvben/vedetta/commit/029152b22a7eab8a6099cfcbbea2a4a268e8e0da))
- **storage**: canonicalize object_references.created_at and storage_audit.ts ([ab2300d](https://github.com/rvben/vedetta/commit/ab2300d882279c736829e18a1adecc88348e4bf0))
- **storage**: canonicalize ordered/compared timestamp columns missed by v2 ([af9d8ff](https://github.com/rvben/vedetta/commit/af9d8ff12639050a4787c9d868c554a5db99b751))
- **storage**: tolerate concurrent legacy column backfill races ([425fa8f](https://github.com/rvben/vedetta/commit/425fa8fa3bf3abaf8fcf6011b0c065ef98af85fa))
- **recording**: treat retain_days <= 0 as unlimited instead of deleting everything ([6387b62](https://github.com/rvben/vedetta/commit/6387b620003cccce7d0a04f1b5a23ec46e86bf73))
- **mqtt**: guard broker dial via custom opener to bypass proxy SSRF gap ([78cb3e1](https://github.com/rvben/vedetta/commit/78cb3e1714d1b214400ba40df2dff60fec8bd7cf))
- **netguard**: enforce SSRF policy at dial time to close DNS-rebinding gap ([e90bbed](https://github.com/rvben/vedetta/commit/e90bbed574d5c2a12a58a139d2851dda1f00a09d))
- **api**: block link-local/metadata targets on RTSP and MQTT test endpoints ([6a67390](https://github.com/rvben/vedetta/commit/6a673903370fcf97f165dd5e21da64257acf33b5))
- **api**: scope MQTT write-only password reuse to stored broker identity ([44f76d8](https://github.com/rvben/vedetta/commit/44f76d810d9f098e371a4e15124dc693e5f8a73b))
- **api**: use stored MQTT password when testing without retyping ([fe3f0b1](https://github.com/rvben/vedetta/commit/fe3f0b1b515394ad917bdea247bab7ab995f548e))
- **api**: make camera and MQTT secrets write-only ([013f949](https://github.com/rvben/vedetta/commit/013f94955ba4dcdae5eba11a5004ac31b3049762))
- **webrtc**: drive browser ICE servers from config, not hardcoded STUN ([4a605c1](https://github.com/rvben/vedetta/commit/4a605c13b9d4069b9b454ae3489a9a21d890e1eb))
- **auth**: release per-IP slot only within its reserved window ([6dfe569](https://github.com/rvben/vedetta/commit/6dfe5699801c10209155c25c65237791cca48adf))
- **auth**: release the per-IP slot on successful auth ([e19d338](https://github.com/rvben/vedetta/commit/e19d3389397217ea6470b65d29f549caded38e79))
- **auth**: reserve aggregate per-IP slot before bcrypt verify ([4a37391](https://github.com/rvben/vedetta/commit/4a37391596a0ab01d7203028a002c9c9eba29f1b))
- **auth**: keep aggregate per-IP login throttle alongside per-account buckets ([833c36f](https://github.com/rvben/vedetta/commit/833c36fd09a9f401d0666216f6612cc21c753775))
- **auth**: scope login rate-limit per (IP, username) ([e0b7d24](https://github.com/rvben/vedetta/commit/e0b7d241bdf6494722fd95a0b914245cb5577d65))
- **api**: reject normalized // paths in login redirect sanitizer ([60491cc](https://github.com/rvben/vedetta/commit/60491cc971d1291609c9896702910a65c56d00b1))
- **api**: prevent open redirect via login next param ([28c6107](https://github.com/rvben/vedetta/commit/28c6107ef0b47ed9b573529370032ed3c24b9e45))
- **api**: add HTTP ReadTimeout to bound slow-body slowloris ([842be0c](https://github.com/rvben/vedetta/commit/842be0ce820c67e4f5b5fa1639dc5cca6ce7048d))
- **api**: sanitize remaining 500 leak paths (embedding + HTML partials) ([863dfc8](https://github.com/rvben/vedetta/commit/863dfc818bb390654cccc4a2faffdf0199d27c43))
- **api**: stop leaking internal error details in HTTP 500 responses ([5963ba3](https://github.com/rvben/vedetta/commit/5963ba3dda0608c69cf1688c28fbb84a5f690423))
- **api**: sanitize event download filename to prevent header injection ([4b78f9d](https://github.com/rvben/vedetta/commit/4b78f9d3cae470bbb0be3cb4ecb07e2f87bd2d36))
- **auth**: scan candidate hashes for a readable bcrypt cost ([edeb574](https://github.com/rvben/vedetta/commit/edeb574cbae23361f2ffff80d47317c792f369f6))
- **auth**: derive dummy hash cost from a hash value, not the live map ([75d04ab](https://github.com/rvben/vedetta/commit/75d04abdfa56d722b016afb1353356a1a4a7cb9f))
- **auth**: publish credential changes before dummy hashing ([d80b7c3](https://github.com/rvben/vedetta/commit/d80b7c34d7b1ff006f4651f773061455bf9a0eb3))
- **auth**: refresh dummy hash on user reload and lock verify reads ([3ca712c](https://github.com/rvben/vedetta/commit/3ca712cf01fd886037148b123064c3113cf42f53))
- **auth**: match dummy bcrypt hash cost to user hashes ([7b7164c](https://github.com/rvben/vedetta/commit/7b7164c2229290d99da4b04a98bb033d3bc0c026))
- **camera**: only confirm a track when its start event is enqueued ([198f472](https://github.com/rvben/vedetta/commit/198f472524fe92b5c8af1526dc8c9678b1037910))
- **auth**: rate-limit change-password by client IP, not empty bucket ([aa664c3](https://github.com/rvben/vedetta/commit/aa664c3dffbf2b4b66000d761073f12abd8c0ee8))
- **camera**: never block the detector on event/event-end sends ([236268e](https://github.com/rvben/vedetta/commit/236268e2bd14b1fcd2f97e73d62bf6a727a8d42b))
- **mqtt**: synchronize subsystems.mqttClient access to fix data race ([1f9c82b](https://github.com/rvben/vedetta/commit/1f9c82b6b57f130393e471f4888f92557c288fea))

### Performance

- **rtsp,stream**: dispatch to consumers without holding locks ([8f3805b](https://github.com/rvben/vedetta/commit/8f3805b57f5ea9861b57721eba7a81e9366d5b25))
- **rtsp**: replace WaitForVideoParams busy-poll with a notification ([87fe934](https://github.com/rvben/vedetta/commit/87fe934b7e39950fd1efed05db592ef8a50246e7))
- **storage**: drop index-defeating replace() from timestamp queries ([574112a](https://github.com/rvben/vedetta/commit/574112ad12516a3c5a0c7364f5d05cc895e62d00))
- **recording**: skip no-op writes in event media reconciliation ([6829495](https://github.com/rvben/vedetta/commit/68294957ef0be7fd0f4764e4acd506cff7bfa177))
- **auth**: compute dummy hash outside the auth mutex ([bda1330](https://github.com/rvben/vedetta/commit/bda1330ee81ab9403be7800192e1118766a9cee6))

## [0.4.4](https://github.com/rvben/vedetta/compare/v0.4.3...v0.4.4) - 2026-05-22

### Fixed

- **media**: hold OpenH264-owned buffer addresses as uintptr to avoid GC heap corruption ([f6e269e](https://github.com/rvben/vedetta/commit/f6e269e2cacaed12f598085c5713be467f80b51a))

## [0.4.3](https://github.com/rvben/vedetta/compare/v0.4.2...v0.4.3) - 2026-05-19

### Added

- **api**: redesign empty state and expose camera names on grid cards ([7e4f715](https://github.com/rvben/vedetta/commit/7e4f7152309c2fd423dc508688362c69a73b56ad))
- **api**: discovery-first Add Camera module ([79f6067](https://github.com/rvben/vedetta/commit/79f606775d29f66b97a7a8ee523e9aed8ab92a25))
- **api**: replace Add Camera modal markup with step container ([7635a1d](https://github.com/rvben/vedetta/commit/7635a1df6ffda17d238ba95109330c390d0e33a0))
- **api**: add styles for discovery-first Add Camera flow ([9d4596d](https://github.com/rvben/vedetta/commit/9d4596debb52c747a2d439ae07b5d69c2c946af8))
- **api**: expose camera discovery handlers at runtime ([8580bda](https://github.com/rvben/vedetta/commit/8580bdafa013457b9eae1d8fc94d36ef91c09043))

### Fixed

- **api**: use hyphen in stream-verified message to match design spec ([8b6d116](https://github.com/rvben/vedetta/commit/8b6d116368cc45e6e288a2e59e166083531ef896))

## [0.4.2](https://github.com/rvben/vedetta/compare/v0.4.1...v0.4.2) - 2026-05-18

### Added

- **api**: redesign storage page with per-camera daily volume ([24a1b48](https://github.com/rvben/vedetta/commit/24a1b48575d1c9b7f7f7118296a1e1a4441fce12))

### Fixed

- **storage**: parse modernc sqlite timestamps in PerDayCameraSegmentBytes ([e09ff55](https://github.com/rvben/vedetta/commit/e09ff5547f6127d9d9d8f5a6cdedf94b37a77a42))
- **api**: allow setDashboardDensity in data-action dispatcher ([7fc89ca](https://github.com/rvben/vedetta/commit/7fc89cae21fb75b7a6cde75d9fd1e3ffa2a133d6))
- **api**: make mobile dashboard stats and tile density usable ([ba92f57](https://github.com/rvben/vedetta/commit/ba92f578564125b00341da46bdda081930e29332))

## [0.4.1](https://github.com/rvben/vedetta/compare/v0.4.0...v0.4.1) - 2026-05-18

### Added

- **ui**: add pure live-cascade decision module ([4c238db](https://github.com/rvben/vedetta/commit/4c238db179738ac7bb08fd22348e5340ebe37a23))
- **stream**: carry H.264 B-frame composition offsets in fMP4 samples ([64f4779](https://github.com/rvben/vedetta/commit/64f4779e6fcd3cc2c4e8ff740f35710fe74e1cf8))
- **stream**: add stream discovery surfaces for camera consumers ([0888847](https://github.com/rvben/vedetta/commit/088884792ea5847c4bb0b90a9ae811feaf5cdefc))
- **stream**: transcode G.711 cameras to AAC for iOS HLS ([f73df66](https://github.com/rvben/vedetta/commit/f73df66fb4871c14f88af3495e7327a911463f4f))
- **web**: play native HLS on iOS, harden snapshot fallback ([464003a](https://github.com/rvben/vedetta/commit/464003aefdd67bd3128100ae8b86735e9fe99f9d))
- **api**: wire live HLS playlist, init, and segment endpoints ([68bdbde](https://github.com/rvben/vedetta/commit/68bdbdeff9b9db57e0cce89a6c01e2555fe336ce))
- **stream**: add live HLS muxer and manager ([90e2529](https://github.com/rvben/vedetta/commit/90e2529a5663c35ad2e64a9e7d0490ee99c6c30f))

### Fixed

- **media**: validate plane geometry at the cgo encoder boundary ([986d2e8](https://github.com/rvben/vedetta/commit/986d2e87a3d1d66de68a5754477c6e271ed024de))
- **detect**: correct SubImage pixel indexing in SCRFD preprocessing ([107ff60](https://github.com/rvben/vedetta/commit/107ff60d01f5fdf17cece8bfb24491b5219dbd4b))
- **media**: bound-check OpenH264 decoded planes against cgo buffer length ([23c59b9](https://github.com/rvben/vedetta/commit/23c59b9f0e342a71236991db5c4e624910d40014))
- **stream**: stabilize MSE frame pacing across fragments ([4b9b77b](https://github.com/rvben/vedetta/commit/4b9b77b4f0a981eb160955137b93e230e8a3348a))
- **ui**: grow self-heal backoff to the cap instead of resetting each cycle ([da784f5](https://github.com/rvben/vedetta/commit/da784f5bbc3d17d1296322e4edd6e7dfdc5779ea))
- **ui**: allow the live-offline Retry button to actually retry ([8faff96](https://github.com/rvben/vedetta/commit/8faff96f83d92f92b68c9798808ff9544226fe58))
- **ui**: show reconnecting state for online cameras and self-heal the cascade ([1b7c2e7](https://github.com/rvben/vedetta/commit/1b7c2e7a5f8d6bc99962e585775c1798be001ba2))
- **ui**: bound WebRTC reconnect so STUN-only cameras fall through to MJPEG ([795bebc](https://github.com/rvben/vedetta/commit/795bebc87e3729d652f8da7711175f0e49d21e18))
- **api**: forward http.Hijacker in request log middleware so WebSocket (MSE) upgrades work ([286f1e0](https://github.com/rvben/vedetta/commit/286f1e0975c42d6316e18178ef4b840df4e4644e))
- **ui**: read paginated envelope in loadZones ([cf64ac9](https://github.com/rvben/vedetta/commit/cf64ac9f5b0bcef0f447afa9a285b06fa46ba05f))
- **stream**: cascade to WebRTC when MSE WebSocket never opens ([bc8624c](https://github.com/rvben/vedetta/commit/bc8624cb704af434215670787f0695c9be5e24c9))
- **hls**: pre-warm live HLS on camera page load to beat iOS cold-start cutoff ([dbe121a](https://github.com/rvben/vedetta/commit/dbe121a9951dd3540aac8e4a3f3988754746e368))
- **recording**: self-heal transient segment-dir failure instead of bricking camera ([921e182](https://github.com/rvben/vedetta/commit/921e182a20641f25685b2fa0d4df7873f70bcd92))
- **hls**: stop iPhone snapshot cascade on suspend/resume and idle reap ([50e4ae9](https://github.com/rvben/vedetta/commit/50e4ae99016d635b6d3514f61f18041d5fb98cde))
- **recording**: bound segment-writer creation so a stalled volume doesn't wedge recording ([e07409e](https://github.com/rvben/vedetta/commit/e07409e93432f4f21a7889e4002522436f164398))
- **camera**: don't gate NVR readiness on cached-snapshot disk I/O ([fc00bae](https://github.com/rvben/vedetta/commit/fc00bae820003b275bf529b7033581cd976f6521))
- **hls**: hold cold playlist request instead of fatal 503 ([34fac3d](https://github.com/rvben/vedetta/commit/34fac3d9e83dfe7349ff12d6f2a9a83fa821d46f))
- **web**: show tapped camera snapshot as backdrop while live stream warms up ([2f4ca93](https://github.com/rvben/vedetta/commit/2f4ca934f77d34ffa287c5478aae7b656d4f78d0))
- **hls**: default iOS native HLS to the sub-stream ([8e4fe83](https://github.com/rvben/vedetta/commit/8e4fe834c2fb2ff63f0e90eef3ebdc24e74e2307))
- **web**: refresh dashboard snapshots after initial grid load ([6fdf0c9](https://github.com/rvben/vedetta/commit/6fdf0c9c7f4c05a54798166394c9db09fa487a95))
- **web**: cascade HLS quality and fall back to snapshots on iOS ([ca44d9f](https://github.com/rvben/vedetta/commit/ca44d9f40d24269649b2c032e155640ae66c053b))
- **api**: emit ETag/Cache-Control so static asset updates reach iOS ([289f261](https://github.com/rvben/vedetta/commit/289f2615859b0887515fc1336e0202a81707f556))
- **stream**: use snapshot-refresh loop for live video on iPhone Safari ([bd973a3](https://github.com/rvben/vedetta/commit/bd973a35fbf03bb847e5eb800c09f53ee274abcc))
- **stream**: drive MJPEG cutover by decoded pixels, not the load event ([9f9382c](https://github.com/rvben/vedetta/commit/9f9382c12d9808312321f77afa11a96f05a4344b))
- **stream**: eliminate iPhone black screen with platform-aware transport ([ce51231](https://github.com/rvben/vedetta/commit/ce51231583f48416f7b61d1bd860e089465022dd))
- **ui**: make event play control an accessible button ([6d45442](https://github.com/rvben/vedetta/commit/6d45442b863c9d0600d738cdd0da90e73c7a3d43))
- **api**: exempt health probes from the readiness gate ([3b448f9](https://github.com/rvben/vedetta/commit/3b448f95f27fa4dd3178351c1babdfc655497432))

### Performance

- **web**: make dashboard snapshot refresh motion-adaptive ([034b7c0](https://github.com/rvben/vedetta/commit/034b7c00c01d867866f4af180a47b231cd2055fd))
- **stream**: paint cached snapshot under a second while MJPEG warms up ([a59c020](https://github.com/rvben/vedetta/commit/a59c0200e2459604a575605cbabb15ac064e4aaa))

## [0.4.0](https://github.com/rvben/vedetta/compare/v0.3.0...v0.4.0) - 2026-05-16

### Added

- **watchdog**: add liveness watchdog and bound crash traceback ([17ec340](https://github.com/rvben/vedetta/commit/17ec340a0194cd7fb811d1e38d5af9fae7fcb615))
- **media**: recover per-packet panics in recording consumer ([48ffb31](https://github.com/rvben/vedetta/commit/48ffb318756b25f0cc16693a9ed22ab3420566ae))

### Fixed

- **rtsp**: clone gortsplib packets before async fan-out ([09355b2](https://github.com/rvben/vedetta/commit/09355b2c2c7357d45546c955e604b45247557a31))

## [0.3.0](https://github.com/rvben/vedetta/compare/v0.2.14...v0.3.0) - 2026-05-13

### Added

- **storage**: add storage-management API, UI, and serialized file-op lock ([c918733](https://github.com/rvben/vedetta/commit/c91873375bc58e51cdf0886b38ef3f8896a75773))
- **api**: link /storage.html from top + bottom navs ([9d4002a](https://github.com/rvben/vedetta/commit/9d4002a62108ae972c91b32b42695c77ec129add))
- **api**: add storage page logic ([5cdbcf1](https://github.com/rvben/vedetta/commit/5cdbcf13b7866508aae580ca750aca94caa2bf0c))
- **api**: add /storage.html scaffold ([db8ec61](https://github.com/rvben/vedetta/commit/db8ec613ae006941db741a0bedad44703a0f2a3e))
- **api**: implement POST /api/storage/cleanup and GET /api/storage/audit ([508bd94](https://github.com/rvben/vedetta/commit/508bd946e90e2644ec17839c636ea80ca64a6df4))
- **api**: implement POST /api/storage/delete with audit + lock contention handling ([8165a8d](https://github.com/rvben/vedetta/commit/8165a8d1d55ca31b9b9f4d66e346611b00eff2e7))
- **api**: implement GET /api/storage ([c68c04b](https://github.com/rvben/vedetta/commit/c68c04be61256115c555956a38da37307ec9dbe7))
- **api**: add /api/storage routes to OpenAPI schema ([2460661](https://github.com/rvben/vedetta/commit/24606616e326a706b40ebffd5cd2093711162b1a))
- **recording**: add TryRunCleanupAsync and audit helpers ([b018ac5](https://github.com/rvben/vedetta/commit/b018ac595ea2d260a100256678c1293963ca77ac))
- **recording**: implement clips, all, and free_bytes delete targets ([1a62bc7](https://github.com/rvben/vedetta/commit/1a62bc7dd6dd64cc22c374778b8dd11b766d4273))
- **recording**: implement DeleteStorage for segments target ([1828c41](https://github.com/rvben/vedetta/commit/1828c41308a3046515f535d16fc29dd0ae681544))
- **recording**: add StorageBreakdown with per-FS accounting ([812fe39](https://github.com/rvben/vedetta/commit/812fe396701303d1b27d3609d4491af99f52873b))
- **recording**: declare storage management types ([d17465b](https://github.com/rvben/vedetta/commit/d17465b9d5e096928c0b65df023137f57a2d35a0))
- **storage**: add storage_audit table for tracking manual deletions ([87dd13f](https://github.com/rvben/vedetta/commit/87dd13f555b3bdb44cae3ac5159ef1fa980fdbcf))
- **recording**: add SaveEventSnapshot serialized via segmentOpMu ([6738852](https://github.com/rvben/vedetta/commit/673885240055a45e5664def64758dd973c167ee8))
- **storage**: add clip query helpers with COALESCE for nullable end_time ([20d9a46](https://github.com/rvben/vedetta/commit/20d9a4670f8d93af34da9314b733a6bcec707da1))
- **storage**: add segment query helpers for scoped deletion ([487d88c](https://github.com/rvben/vedetta/commit/487d88c01b521fca379dd310a92f559fd96e25e4))
- **recording**: add CurrentSegmentPaths exposing active writers ([7845c85](https://github.com/rvben/vedetta/commit/7845c855310f38757061e4671c2ce87d62035e83))
- **media**: expose Camera() and CurrentSegmentPath() on RecordingConsumer ([943c436](https://github.com/rvben/vedetta/commit/943c436e0508244298ecd010076a6f797f64dc96))

### Fixed

- **recording**: propagate open-segment protection in target=all deletes ([c981bf1](https://github.com/rvben/vedetta/commit/c981bf12dfa973730c1e0062d8f1b18f09420a21))

## [0.2.14](https://github.com/rvben/vedetta/compare/v0.2.13...v0.2.14) - 2026-05-11

### Fixed

- **webrtc**: keep m=audio in BUNDLE so ICE candidates land on an active m-line ([5b1cc1b](https://github.com/rvben/vedetta/commit/5b1cc1b0b79e5ab61fbb6f10649954892e07d7c7))

## [0.2.13](https://github.com/rvben/vedetta/compare/v0.2.12...v0.2.13) - 2026-05-11

### Fixed

- **webrtc**: cap answer level_idc at 3.1 so Chrome can allocate decoder ([44152d6](https://github.com/rvben/vedetta/commit/44152d647ad287db7af2797570e39d37b4c1791d))

## [0.2.12](https://github.com/rvben/vedetta/compare/v0.2.11...v0.2.12) - 2026-05-11

### Fixed

- **webrtc**: sniff in-band SPS/PPS so cameras without sprop-parameter-sets negotiate the right profile ([2ad06b0](https://github.com/rvben/vedetta/commit/2ad06b006a823d167d8b56e7426f50068042cc94))

## [0.2.11](https://github.com/rvben/vedetta/compare/v0.2.10...v0.2.11) - 2026-05-11

### Fixed

- **webrtc**: clear marker bit on SPS/PPS so Chrome assembles full access unit ([117eb82](https://github.com/rvben/vedetta/commit/117eb82fd5c3793953cec8b591ba3db01717ca4e))

## [0.2.10](https://github.com/rvben/vedetta/compare/v0.2.9...v0.2.10) - 2026-05-11

### Fixed

- **webrtc**: default to sub-stream so Chrome can decode ([f482025](https://github.com/rvben/vedetta/commit/f482025315850845c8b3c9973ca3bdf05da46ca3))

## [0.2.9](https://github.com/rvben/vedetta/compare/v0.2.8...v0.2.9) - 2026-05-11

### Fixed

- **webrtc**: advertise rtcp-fb in answer SDP so Chrome decoder commits ([38fc49b](https://github.com/rvben/vedetta/commit/38fc49b568f74780f66fcb74daf8da66e661bf1d))

## [0.2.8](https://github.com/rvben/vedetta/compare/v0.2.7...v0.2.8) - 2026-05-11

### Fixed

- **webrtc**: rewrite answer profile-level-id to match camera bitstream ([9281a60](https://github.com/rvben/vedetta/commit/9281a6016e733c6706491d0d993d2385ca8cf40b))

## [0.2.7](https://github.com/rvben/vedetta/compare/v0.2.6...v0.2.7) - 2026-05-11

### Fixed

- **webrtc**: refragment oversized FU-A and disable NACK responder ([7a182ef](https://github.com/rvben/vedetta/commit/7a182efc2dcaab98f1ac3d35d31f7cf06d2f7b04))

## [0.2.6](https://github.com/rvben/vedetta/compare/v0.2.5...v0.2.6) - 2026-05-11

### Fixed

- **camera**: derive Online from frame freshness, not raw RTSP Connected() ([2116259](https://github.com/rvben/vedetta/commit/21162590f488cc646500260fc7fcbb34adcbae7d))

## [0.2.5](https://github.com/rvben/vedetta/compare/v0.2.4...v0.2.5) - 2026-05-11

### Fixed

- **api**: snapshot endpoint serves live frames with no-cache headers ([3816adc](https://github.com/rvben/vedetta/commit/3816adc46ef662d86aac15732fd5249b809a1c4d))

## [0.2.4](https://github.com/rvben/vedetta/compare/v0.2.3...v0.2.4) - 2026-05-08

### Added

- **stream**: allow ?quality=low to route MSE to the detect substream ([07af2dc](https://github.com/rvben/vedetta/commit/07af2dc06aae0b6458dc3b00d8603b39d64bf8d8))
- **web**: clamp name popover and show click-to-start on autoplay block ([b4d7970](https://github.com/rvben/vedetta/commit/b4d79702f3a69df6e922e6f9655428678078d3a6))
- **api**: expose source_fps in camera detail response ([309ea14](https://github.com/rvben/vedetta/commit/309ea14b452e0f35b8a8c335d786ba2e0426fb23))

### Performance

- **stream**: halve MSE fragment cadence and normalize sample durations ([5273bd7](https://github.com/rvben/vedetta/commit/5273bd7c45667be2607bfbdafb72fa9abad7d8cd))

## [0.2.3](https://github.com/rvben/vedetta/compare/v0.2.2...v0.2.3) - 2026-05-08

### Fixed

- **web**: drift-correct MSE playback to smooth out live preview ([d739707](https://github.com/rvben/vedetta/commit/d7397073526d3ec871cf44f43ce08dec05a89997))

## [0.2.2](https://github.com/rvben/vedetta/compare/v0.2.1...v0.2.2) - 2026-05-08

### Added

- **web**: click a tracked object on the live view to name it ([1c3e3e0](https://github.com/rvben/vedetta/commit/1c3e3e09c6e60463f8375ff95ab9df2723ce1ce7))
- **api**: add POST /api/cameras/{name}/objects to name a live track ([8c1dcd4](https://github.com/rvben/vedetta/commit/8c1dcd4484fa7f18e96ac64e1ebad511ce249484))
- **main**: push matched object names back to the camera overlay ([b613eed](https://github.com/rvben/vedetta/commit/b613eed4ce3e7e55a589f48fbf6cca1e081b1932))
- **camera**: track per-object display names and expose live frame ([27c05fa](https://github.com/rvben/vedetta/commit/27c05fa94693ca3b16b21f5ca83bb928253fff5e))

### Fixed

- **stream**: batch fMP4 fragments and stabilize live-edge auto-seek ([6c6af5d](https://github.com/rvben/vedetta/commit/6c6af5d057eeb7eafb31e4f3cec76bcb59fda97a))
- **camera**: age tracks every frame so events resume after quiet periods ([e340981](https://github.com/rvben/vedetta/commit/e34098190c7e6aafdd3549f89a3712a358fab887))
- **detect**: preserve stationary tracks across motion-gated quiet periods ([3447041](https://github.com/rvben/vedetta/commit/3447041d4123a8292e80b2a342a9dac520f3e1e6))
- **media**: rewrite fMP4 trim/concat to handle multi-track moofs ([668c025](https://github.com/rvben/vedetta/commit/668c025be1a549e13ef228acea929120860ee955))

## [0.2.1](https://github.com/rvben/vedetta/compare/v0.2.0...v0.2.1) - 2026-05-02

### Added

- **web**: pick a tracked-object thumbnail from sightings or references ([e3d7892](https://github.com/rvben/vedetta/commit/e3d7892a7f0b91d811ca1c5ae6b96115490227bd))
- **web**: redesigned tracked objects page ([1d0064a](https://github.com/rvben/vedetta/commit/1d0064a8fdbb7c495461660514dcf4b565c3f013))
- **web**: mask RTSP credentials in camera settings with reveal toggle ([a78eec4](https://github.com/rvben/vedetta/commit/a78eec4a4c1d9df2b627aec087b47d792849d9e4))
- **web**: camera page live view UX improvements ([b473ff6](https://github.com/rvben/vedetta/commit/b473ff62ff3c7b5729bb1480f25320fc904a942f))
- **web**: add tile-density selector to dashboard ([c2c3f11](https://github.com/rvben/vedetta/commit/c2c3f1120c972c3c2fafb6669c3cedec461bc994))
- **web**: auto-fill camera grid layout ([f309a25](https://github.com/rvben/vedetta/commit/f309a25cefb75a4552f82b1bf6e39cc5e11b8f09))
- **web**: cross-cutting UX improvements (W2.4) ([ad60c86](https://github.com/rvben/vedetta/commit/ad60c86344d96beb5749f1db537c3693a4f78032))
- **web**: fix event-card badge legibility and add badge legend ([17991ee](https://github.com/rvben/vedetta/commit/17991eeec5d548512ca33abe01f7de09f9278cc0))
- **web**: replace cryptic identity codes with readable labels on event tiles ([b5d5256](https://github.com/rvben/vedetta/commit/b5d525677086d40c1675b891487f9a8481018448))
- **web**: eager-load camera snapshots on dashboard tiles ([953baf7](https://github.com/rvben/vedetta/commit/953baf78675b02869e9d1fe029a3b0d3b23ddb53))
- **web**: enrich PWA manifest with id, description, screenshots ([8a787c5](https://github.com/rvben/vedetta/commit/8a787c5aab66ac9d69dbb0916b54afd6a093bae8))
- **web**: add PWA chrome to login page ([6769c44](https://github.com/rvben/vedetta/commit/6769c4458364d78298e70704b3b81f60ef6be085))
- **api**: redirect extensionless paths and serve app-shell 404 ([6a32fb5](https://github.com/rvben/vedetta/commit/6a32fb575177c7df219024457bef28d0edb08d74))
- **web**: add sticky jump nav to settings page ([1dd214c](https://github.com/rvben/vedetta/commit/1dd214c7ccbc9459bb77427168e8ff54e62c1782))
- **web**: bump mobile touch targets and landscape camera layout ([9242bc1](https://github.com/rvben/vedetta/commit/9242bc1a7efa07c2df5bc30c11860d48b8823151))
- **web**: live bounding-box overlay on camera view ([b4d5b2e](https://github.com/rvben/vedetta/commit/b4d5b2e183a93cd1eec087a10beda59f859dd188))
- **web**: RTSP test-connection button in camera forms ([f01aca0](https://github.com/rvben/vedetta/commit/f01aca0295ec9563660bb6f10892b0eb2175794d))
- **web**: UI/UX pass across dashboard, streaming, mobile, and PWA ([3138f9e](https://github.com/rvben/vedetta/commit/3138f9e76cebc1e2c17e19cdcb916d62cb2d78d4))

### Fixed

- **mqtt**: assign client before connect to avoid OnConnect race ([de52000](https://github.com/rvben/vedetta/commit/de520008c7b7b19486b11a1e5987e462499a89e7))
- **web**: auto-regenerate missing tracked-object crops from recent sightings ([55848ca](https://github.com/rvben/vedetta/commit/55848caea423012d7924085b716f064148d77530))
- **web**: tracked-object thumbnails fall back when crop file missing ([b0fa986](https://github.com/rvben/vedetta/commit/b0fa986a54dbbf0ae4264c6c44ac0c9923ea3e15))
- **web**: camera events badge shows true total via X-Total-Count ([695f469](https://github.com/rvben/vedetta/commit/695f4695422f1bb8fb27169bfbcede6d70753264))
- **web**: cap camera-page events at 24 with View all link ([114254e](https://github.com/rvben/vedetta/commit/114254ea6bfc1baf825f482717d3a6ae59190647))
- **web**: proceed optimistically on non-404 camera validation errors ([fbbfbaa](https://github.com/rvben/vedetta/commit/fbbfbaa3cfdb6e8ebeab196524626232c83be428))
- **web**: prevent event clip player from collapsing before metadata loads ([9f8855f](https://github.com/rvben/vedetta/commit/9f8855f52917e7973d712b81a97ef61c8e051e53))
- **web**: move notifications card styles out of body innerHTML ([8fe4456](https://github.com/rvben/vedetta/commit/8fe44562c891afc693597d308d629872dcf1d3e1))

## [0.2.0](https://github.com/rvben/vedetta/compare/v0.1.0...v0.2.0) - 2026-04-19

### Added

- **events**: fall back to local disk for snapshots when primary volume is full ([b94761e](https://github.com/rvben/vedetta/commit/b94761e2d86e19cc3a18fa91e75272dfe7d462f6))
- **mqtt**: publish disk status and recording-paused sensors with HA discovery ([5da2bc1](https://github.com/rvben/vedetta/commit/5da2bc1872ee2ed27845c3184ef72a9f2c60b291))
- **recording**: configurable recompress interval and size-priority selection ([c82f956](https://github.com/rvben/vedetta/commit/c82f95664143b49ae46dffdfd924c47c560895c5))
- **recording**: support per-camera retain_days override ([2468cff](https://github.com/rvben/vedetta/commit/2468cff818846c70788a779dceac4f507fcfc99f))
- **recording**: add floor-breaking emergency cleanup to prevent silent recording pause ([c1dd3a3](https://github.com/rvben/vedetta/commit/c1dd3a3539c544e6c20be9fa6406f96b62412c4c))
- **media**: make disk-free threshold configurable and dynamic ([9f07611](https://github.com/rvben/vedetta/commit/9f07611407d773190841d5bd97923cc8bc32a2e0))
- **storage**: add queries for emergency cleanup floor and size-ordered recompress candidates ([f084557](https://github.com/rvben/vedetta/commit/f084557e7f59d96d7afab95d8f0b0715463e7844))
- **config**: add min_disk_free, urgent_cleanup, tiered_storage interval/priority, per-camera retain_days ([0708cd4](https://github.com/rvben/vedetta/commit/0708cd4c27fa1ddc676c3638ee32f6ea244e11ea))
- **config**: add notifications.vapid_subscriber config field ([b0348c4](https://github.com/rvben/vedetta/commit/b0348c4cbbae451d0f47370d72393ca4977b935e))
- **notify**: derive friendly camera names for notification titles ([ff19099](https://github.com/rvben/vedetta/commit/ff190992c48e8c20c32aa96fcd02b3e09666f3d7))
- **api**: add public GET /api/push/snapshot/{id} handler ([3230ecd](https://github.com/rvben/vedetta/commit/3230ecd3983415535fc3f489da8e92d8b59cf9f8))
- **notify**: use signed URLs in push payload image field ([32d64cd](https://github.com/rvben/vedetta/commit/32d64cd33c0608941a7dbadc9dd12c207a41d4e2))
- **notify**: add HMAC-signed snapshot URL signer ([63145d5](https://github.com/rvben/vedetta/commit/63145d5dd5d47ca58955cc8e792cf8d57e5a39ca))
- **ui**: add Notifications section with Enable, prefs matrix, devices list ([669c985](https://github.com/rvben/vedetta/commit/669c985424ce20ebfd7be5d35a7a411a7220e24e))
- **ui**: register service worker, install hint, Remember-me promotion ([d48d7d5](https://github.com/rvben/vedetta/commit/d48d7d56e94d090df437ec5a239109f75cf712c6))
- **ui**: add PWA manifest link to every HTML page ([c24689c](https://github.com/rvben/vedetta/commit/c24689cee433f53da39ff5b4a9551758b5425913))
- **ui**: add PWA manifest and service worker ([4b41233](https://github.com/rvben/vedetta/commit/4b41233928e3064f4eaf2545ec03ef3c832f7324))
- **ui**: add PWA app icons ([a6ae5ff](https://github.com/rvben/vedetta/commit/a6ae5ff9cdbfcbf7d0a31647523aa4ca399c1113))
- **api**: expose notify metrics through /metrics ([38e1d44](https://github.com/rvben/vedetta/commit/38e1d4469dda047306d7136f4faba164057a0058))
- **vedetta**: enqueue events to push dispatcher after snapshot save ([c2c12fd](https://github.com/rvben/vedetta/commit/c2c12fda2211dd381c3f0b9d1783ab4f7778aebc))
- **vedetta**: instantiate notification dispatcher at startup ([fa3f108](https://github.com/rvben/vedetta/commit/fa3f108bf2266e23d77343923e211f0937785f2c))
- **api**: register /api/push/* routes on server mux ([8199925](https://github.com/rvben/vedetta/commit/81999258d4602664d6f4711559dfccc4690936e3))
- **api**: add push subscription, prefs, and test handlers ([5a06298](https://github.com/rvben/vedetta/commit/5a0629849b58fa9677708b7379bbb8ed2f1c8e0f))
- **notify**: add WebPushSender production adapter ([e8e10c7](https://github.com/rvben/vedetta/commit/e8e10c710fdc3e68106adedc940e61b2ecbd10e3))
- **notify**: scaffold NotificationDispatcher with Store interface ([3c7ded0](https://github.com/rvben/vedetta/commit/3c7ded0186a79b6c5e5ce71957362764ab984a5f))
- **notify**: add lock-free metrics counters ([318e5d4](https://github.com/rvben/vedetta/commit/318e5d4b4d04f624da02cf2630e7881b47e72eed))
- **notify**: add push payload builder ([c6ee1c8](https://github.com/rvben/vedetta/commit/c6ee1c86ba7b961816e201dad21896f9797c2216))
- **notify**: add CooldownCache with injectable clock ([00853f8](https://github.com/rvben/vedetta/commit/00853f83f94fe60a7d7ba5f0e3079388e66548f5))
- **notify**: add subscription endpoint and key validation ([6c98cd8](https://github.com/rvben/vedetta/commit/6c98cd88389227ec1f27e8791575b3d39a4eb374))
- **notify**: add VAPID keypair load/generate with fail-closed policy ([164f43d](https://github.com/rvben/vedetta/commit/164f43de98ef965a68f869e56a8aba89c412a67e))
- **storage**: add notification_prefs CRUD with default-enabled semantics ([6fd3b7a](https://github.com/rvben/vedetta/commit/6fd3b7a2a4dac2a2225681e055c7944d931ed379))
- **storage**: add push_subscriptions CRUD ([8fcf243](https://github.com/rvben/vedetta/commit/8fcf2431c3ab36ffd959d03ce0307301987ce61f))
- **storage**: add push_subscriptions and notification_prefs tables ([ebe2ed4](https://github.com/rvben/vedetta/commit/ebe2ed45fbf55586f8f498cafbc570c0247b0c03))
- **storage**: add steady-state and linear projection to StorageStats ([eae759f](https://github.com/rvben/vedetta/commit/eae759f67bf76fe8db98e33c46576209f3add031))
- **codecs**: auto-install OpenH264 on startup when missing ([843f123](https://github.com/rvben/vedetta/commit/843f12327a66a8cc82826937b79886d40f10a84c))
- **health**: expose detection subsystem status in /api/health ([4d14796](https://github.com/rvben/vedetta/commit/4d14796699b044567d3d1bdb0c0f94ccde1bde30))
- **ui**: add stop/start toggle on dashboard camera cards ([10a42ff](https://github.com/rvben/vedetta/commit/10a42ffca4fb79246e0bf7f1d9555c79a2c23800))
- **mqtt**: publish 'stopped' status for stopped cameras ([4b58218](https://github.com/rvben/vedetta/commit/4b5821886126c5c0b1e901f955415f2db9b9110a))
- **api**: add stop/start camera endpoints with OpenAPI spec ([22879cd](https://github.com/rvben/vedetta/commit/22879cdbb5d9b32ee1be5e0cbd9f31e45cc12fcb))
- skip stopped cameras on startup ([608dcd6](https://github.com/rvben/vedetta/commit/608dcd6161445a5066fc1628bd1572eeec056879))
- **recording**: per-camera stop/start recording ([6e66f0d](https://github.com/rvben/vedetta/commit/6e66f0d4d03d287bc8dd2be0a2f63cdf9c8b5071))
- **camera**: per-camera context with StopCamera/StartCamera/IsStopped ([75a809e](https://github.com/rvben/vedetta/commit/75a809e27bb1c49576264ba867654feb57613545))
- **storage**: add camera stopped state persistence via kv_store ([7fc741c](https://github.com/rvben/vedetta/commit/7fc741c5608661e94f665a9e6cd2655092b6a243))
- **mqtt**: publish object counts and event end messages ([90417ff](https://github.com/rvben/vedetta/commit/90417ff2820527f41375fda8a9eb92dff44703f4))
- **mqtt**: add PublishObjectCount for per-camera per-label object counts ([27571d0](https://github.com/rvben/vedetta/commit/27571d059616129d400186208d9883553cd43d33))
- **ui**: add camera management page with CRUD operations ([bdfe140](https://github.com/rvben/vedetta/commit/bdfe140628c3c90d14d8a09b462f2046d7cce042))
- **ui**: add recording, detection, and password cards to settings page ([2e5b971](https://github.com/rvben/vedetta/commit/2e5b9715ee1d4120e4d9ce2a838f2e47f6eaf86b))
- **api**: add camera management CRUD endpoints ([f115070](https://github.com/rvben/vedetta/commit/f1150709e97f7b873b15f49989bf48735713391c))
- **api**: add password change and auth info endpoints ([771585a](https://github.com/rvben/vedetta/commit/771585a81d2f0e5e2213dfd2d2b2918ffd79fde6))
- **api**: add recording and detection settings endpoints with hot-reload ([c09c3a0](https://github.com/rvben/vedetta/commit/c09c3a0c4ec1efbd927aed769066eb41ee5ae04b))
- **api**: wire detector and recording config into server ([527426e](https://github.com/rvben/vedetta/commit/527426e39132ddbc49dbb029a6e9af5bc22833fc))
- **config**: add camera update/remove and auth password config writers ([e13856a](https://github.com/rvben/vedetta/commit/e13856abe16ee7f735eb5d9d479817917691bc31))
- **detect**: add SetScoreThreshold and SetLabels for hot-reload ([df1fee3](https://github.com/rvben/vedetta/commit/df1fee3fe70c3a55a5b154efdde3b19d8263bf4b))
- **config**: add UpdateRecording and UpdateDetect config writers ([027cb30](https://github.com/rvben/vedetta/commit/027cb30cb10b28fa1c1147a45cbe774f59b355bf))
- **ui**: add Settings link to navigation across all pages ([f02327e](https://github.com/rvben/vedetta/commit/f02327e21848571e0943a64751ff6be99cca53f2))
- **ui**: add settings page with MQTT config and update checker UI ([0cf9ceb](https://github.com/rvben/vedetta/commit/0cf9ceb947aaccd67e36a302ec2422a64ee8d2b8))
- **api**: show update badge on system page when new version available ([dd76387](https://github.com/rvben/vedetta/commit/dd76387ec29f312be38d11c5a4a1af416dfe2d0c))
- **api**: add settings endpoints for MQTT config and update checking ([d43727f](https://github.com/rvben/vedetta/commit/d43727fcf6e9e8eec2701750a8189bb5f42a09e2))
- wire update checker into startup and server ([f709ab7](https://github.com/rvben/vedetta/commit/f709ab7fdad6a94195aad53e19cb4ba2a329896f))
- **config**: add UpdateMQTT and UpdateUpdates for live config writes ([f272043](https://github.com/rvben/vedetta/commit/f2720431b36ed09218d091bd9e745bf158866fad))
- **mqtt**: add mDNS broker discovery via zeroconf ([9946f33](https://github.com/rvben/vedetta/commit/9946f3340f369191cb893280cbb48c95c79cfc42))
- **update**: add GitHub release checker with semver comparison ([d7aec4c](https://github.com/rvben/vedetta/commit/d7aec4c003fbd4d2a7f9ad4d51a32b0c7eac8df7))
- **config**: add updates section for update checker settings ([6f94eca](https://github.com/rvben/vedetta/commit/6f94eca90085d1580c42bc0c9bc8fb2d29beabea))
- **storage**: add kv_store table for persistent settings ([4fbae12](https://github.com/rvben/vedetta/commit/4fbae1207ef4f24743d47d437af1e5b60b2a80fc))
- **ui**: harden frontend with action whitelisting and setup token flow ([96c91ad](https://github.com/rvben/vedetta/commit/96c91add1511cf02af5e48a9012e805e4a27a717))
- **api**: add setup token security, body limits, CSP headers, and version embedding ([641199d](https://github.com/rvben/vedetta/commit/641199dd3672d9edad6373d35e9f50f99583ba70))
- add config validation, auth token scoping, and recording improvements ([f566525](https://github.com/rvben/vedetta/commit/f566525cd3ffe01a22e97829787b7247dd60e239))
- **api**: wire ServerInterface for compile-time API contract enforcement ([95b7680](https://github.com/rvben/vedetta/commit/95b7680c5b2e39ba431dc969060b536e76fcb55c))
- **api**: add GET /api/tokens endpoint for listing API tokens ([f556396](https://github.com/rvben/vedetta/commit/f556396165144cdabb142db014c04d811e9d1a54))
- **api**: add object and streaming endpoints to OpenAPI spec ([4fd8236](https://github.com/rvben/vedetta/commit/4fd82367e1d23c851a3d2cd2263e154dbbe6693c))
- **api**: add people and face endpoints to OpenAPI spec ([d44bd60](https://github.com/rvben/vedetta/commit/d44bd60570cba3e7c22b1c36d70c7d1e6406f020))
- **api**: add recording endpoints to OpenAPI spec, expose segment IDs ([5248938](https://github.com/rvben/vedetta/commit/5248938f40d77061544afb56e8ec36506f5a517a))
- **api**: add event endpoints to OpenAPI spec, add since filter and list envelope ([7e6e8f7](https://github.com/rvben/vedetta/commit/7e6e8f7f6d1e1c5bbf3721b5e21028e0a97b1ae3))
- **api**: add camera endpoints to OpenAPI spec, add GET /api/cameras/{name} ([f06576e](https://github.com/rvben/vedetta/commit/f06576e7b2311ab7dacf026adef8bf0364049d1e))
- **api**: add auth endpoints to OpenAPI spec with contract tests ([23d9c5a](https://github.com/rvben/vedetta/commit/23d9c5ae0ee618a38f5a14c79a8ce23ff511ef44))
- **api**: serve OpenAPI spec at /api/openapi.json with contract test harness ([e10bf20](https://github.com/rvben/vedetta/commit/e10bf20e2c6f72d6c77b51fe0a1f2396e54eeab4))
- **api**: add OpenAPI 3.1 spec skeleton with health endpoints ([412d8b5](https://github.com/rvben/vedetta/commit/412d8b5ca3fde7c823e1b49e7ef1263c9a0a80d9))
- **ui**: improve account modal for proxy and session auth ([2f5830f](https://github.com/rvben/vedetta/commit/2f5830ff238c1719263ec15edaa095ce246bf71a))
- **ui**: hide change password form for proxy-authenticated users ([5367ccf](https://github.com/rvben/vedetta/commit/5367ccf1975bd4a3d366378e594c345532c11082))
- **api**: redirect already-authenticated users away from login page ([2992b21](https://github.com/rvben/vedetta/commit/2992b2178a5fa477db21c144f521fbcd769ede6d))
- **auth**: add debug logging for proxy authentication ([8a335c4](https://github.com/rvben/vedetta/commit/8a335c4ed7b4ffd9459ee40a31aaf5b64d78f304))
- **auth**: add reverse proxy header authentication ([6688898](https://github.com/rvben/vedetta/commit/6688898ff1a2fde3a397ebeeee92b1c42a6db449))
- **config**: add auth.proxy.header config for reverse proxy authentication ([a105e78](https://github.com/rvben/vedetta/commit/a105e783dda0b2d1b345a3039191aec6f2af0f6c))
- **recording**: add manual recompression trigger ([70367a3](https://github.com/rvben/vedetta/commit/70367a34a50772e204ff8b0141e680f80831a513))
- **recording**: expose recompression runtime stats (last_run, segments, bytes_reclaimed) ([030b5f9](https://github.com/rvben/vedetta/commit/030b5f9fa0e5f3f61584cae8f6feba911aa07e4f))
- **recording**: clean stale .tmp files from interrupted recompression on startup ([6fa80a7](https://github.com/rvben/vedetta/commit/6fa80a70755f2f56648983bdc817ee8e63a0039b))
- **recording**: wire up tiered storage recompressor and expose stats ([14291bf](https://github.com/rvben/vedetta/commit/14291bfc8ec10f2f0051c5d412e70761de48e1c6))
- **recording**: add tiered storage recompression job ([20d7019](https://github.com/rvben/vedetta/commit/20d7019ccb296113ba2bf7ee8a9d434c0f2d0636))
- **media**: implement TranscodeSegment for tiered storage recompression ([529763b](https://github.com/rvben/vedetta/commit/529763bc162bdc0b98b1fab9eebae728eb87999c))
- **media**: add resolution check and source resolution reader for transcoder ([de97cbd](https://github.com/rvben/vedetta/commit/de97cbda69f375fd94bc225e46a1b5180d0887a6))
- **media**: add YCbCr scaler for tiered storage transcoding ([341a7e9](https://github.com/rvben/vedetta/commit/341a7e90a074689ddb98f2039d8bcddd0f3cc0f1))
- **media**: track HLS segment paths in use for recompression safety ([a395a6d](https://github.com/rvben/vedetta/commit/a395a6de09ce890a3b8d36b31a54dc6dd81206d6))
- **config**: add TieredStorageConfig with per-camera overrides and schedule parsing ([6d0d5eb](https://github.com/rvben/vedetta/commit/6d0d5eb9b317b0872aa4ec5b8f1eb0f399a94ecb))
- **storage**: add recompression tracking columns to segments ([e3ea738](https://github.com/rvben/vedetta/commit/e3ea738a31f01690d67c3665523e1d9dc246a9e2))
- server-side re-segmenting for HLS playback ([407540b](https://github.com/rvben/vedetta/commit/407540b3091ff0b9a70d11f48181b76dcff3eb97))
- HLS playback for recorded video ([b1c9c6a](https://github.com/rvben/vedetta/commit/b1c9c6a989803b3a624aac7d66b2162bcc637db6))
- **ui**: snap to event start time when clicking event bars on timeline ([6ec2935](https://github.com/rvben/vedetta/commit/6ec29352be716ca415f1e23b1c2f1d9f1d2810bf))
- **ui**: render waveform timeline with motion activity data ([69cc05b](https://github.com/rvben/vedetta/commit/69cc05bc6b4430c915877254688caae12e2d25e0))
- **ui**: add canvas element and CSS for waveform timeline ([a9ca55f](https://github.com/rvben/vedetta/commit/a9ca55f1402c06f5fcb40b6cb144ce9367dbad9d))
- **api**: add motion activity and event end_time to timeline response ([bf3ef76](https://github.com/rvben/vedetta/commit/bf3ef76a97fd919aa94ae01ee493a6f1cbbbfe61))
- wire motion activity channel from cameras to DB ([2755e56](https://github.com/rvben/vedetta/commit/2755e566064893e860b3d68171ed699429b07a5a))
- **recording**: add motion activity retention cleanup ([3d779f4](https://github.com/rvben/vedetta/commit/3d779f4c87729518cd7ea46b4925824c71c8186e))
- **camera**: add per-minute motion activity accumulation ([42435b8](https://github.com/rvben/vedetta/commit/42435b82a64d4344fe5a0ea440d4387d25187782))
- **storage**: add motion_activity table and methods ([b377672](https://github.com/rvben/vedetta/commit/b3776724e59ce6bf5257e5614662f26fe167b88e))
- **detect**: expose frame coverage from motion detector ([338d5d7](https://github.com/rvben/vedetta/commit/338d5d7e6dcf629d730f0438a625efa6dac90ccd))
- **ui**: YouTube-style video control bar overlay ([92d18ee](https://github.com/rvben/vedetta/commit/92d18ee7ebc91b2ec221077f2dc3562451d77e55))
- **ui**: click video to pause/play with centered indicator ([63354cc](https://github.com/rvben/vedetta/commit/63354ccc4d120ebcff28e35955359f3ec65810e2))
- **ui**: add pause/play button to video overlay ([1d4b27c](https://github.com/rvben/vedetta/commit/1d4b27cf8cc155fb8ca275794994b67a45bec627))
- **ui**: hide stream toolbar, show only during playback ([4182f38](https://github.com/rvben/vedetta/commit/4182f38f00cadec8508c611e495b8384d4b7d303))
- **ui**: move mute/PiP/fullscreen to video hover overlay ([521f25f](https://github.com/rvben/vedetta/commit/521f25fc9661b97cfd5a25ba2bd19c13b189d347))
- **ptz**: add press-and-hold controls and keyboard shortcuts ([76363c2](https://github.com/rvben/vedetta/commit/76363c213584e774cfc05470af6224b934eff6a8))
- **ptz**: probe cameras for PTZ support at startup ([599b69d](https://github.com/rvben/vedetta/commit/599b69d8e16e2d6fce46c7d993ae121dfe410bf5))
- **ptz**: add D-pad and zoom controls UI ([89fc9e1](https://github.com/rvben/vedetta/commit/89fc9e1e1ec89f66886d6641ad4a783d1e62cc60))
- **ptz**: add PTZ API endpoint and camera status field ([3401881](https://github.com/rvben/vedetta/commit/3401881090f8bc140627367e94ff273cf85c3a0e))
- **ptz**: ContinuousMove and Stop SOAP commands ([eb1b920](https://github.com/rvben/vedetta/commit/eb1b9204132c74dc184fc17b80b95a32c60db252))
- **ptz**: ONVIF capability detection with auth fallback ([eaf2f37](https://github.com/rvben/vedetta/commit/eaf2f3716cd5054766ae980a99724eb746036d5f))
- **ptz**: add ONVIF SOAP helpers and WS-Security auth ([1f6748c](https://github.com/rvben/vedetta/commit/1f6748c2aba3c91ebaf6f7454b15f14c1fac86bf))
- **auth**: password change and remember-me sessions ([6e4eabe](https://github.com/rvben/vedetta/commit/6e4eabeafc81b5cdc4dff9a3f026a9200e809e83))
- **auth**: add remember-me with extended session TTL ([e5ed8b7](https://github.com/rvben/vedetta/commit/e5ed8b712f9344f13be496540297eb730cdf6641))
- **auth**: add change password endpoint ([18ff115](https://github.com/rvben/vedetta/commit/18ff115297ce949a4aeae1421fa42b1c03ebd827))
- web-based onboarding flow ([7081a33](https://github.com/rvben/vedetta/commit/7081a336d540c482d52e5d102878c56c09c7b10c))
- **onboarding**: add camera thumbnails and editable names during discovery ([fdb2372](https://github.com/rvben/vedetta/commit/fdb237218c7bd13be51108f52946b95f2dd3f0c1))
- **ui**: empty dashboard state with discover and add-manual CTAs ([7204e7f](https://github.com/rvben/vedetta/commit/7204e7f453fb1a8528814487c91a8955c12608af))
- **ui**: add onboarding setup page with account creation and camera discovery ([2e1b400](https://github.com/rvben/vedetta/commit/2e1b4001d67e025706e2e830edd189f8a4621224))
- **camera**: add hot-add support to camera manager ([eec8462](https://github.com/rvben/vedetta/commit/eec84621a448157cb10012d44ec1b8a177468ddb))
- setup mode startup path in main ([d578eb9](https://github.com/rvben/vedetta/commit/d578eb91dd7c8e7a1479739bff5c4223578aa6c1))
- **api**: add setup mode routing and transition to full mode ([2101e7a](https://github.com/rvben/vedetta/commit/2101e7aa0a390c9af50f367d0714dcdb2ea4d3a0))
- **api**: add setup mode handlers for onboarding ([8d734d1](https://github.com/rvben/vedetta/commit/8d734d10abe83149d7dd49cdc239d9a4a3f74f3d))
- **auth**: seed config users into DB on startup ([eeea997](https://github.com/rvben/vedetta/commit/eeea997b76ac2a50399d28008f1aa2f6d6fe8f0a))
- **auth**: add DB-primary auth with NewFromDB constructor ([fae08b5](https://github.com/rvben/vedetta/commit/fae08b55d8b3827b60588782e26a121ea3820665))
- **storage**: add auth_users table with save, seed, and list ([9a6e2e8](https://github.com/rvben/vedetta/commit/9a6e2e87f92a57d7aec16e07c4960514624c9277))
- **config**: add config file writing with yaml.Node preservation ([109198a](https://github.com/rvben/vedetta/commit/109198a493216462bf806fcdbef5237c1fbea646))
- **config**: add LoadOrDefault and allow zero cameras ([84387b2](https://github.com/rvben/vedetta/commit/84387b26113003db26609d143a6f67243dd9afdc))
- **ui**: unify unmatched faces and appearances into same identification modal ([d347f10](https://github.com/rvben/vedetta/commit/d347f104c378589df4e1d9590538bd030cc9b9cf))
- **api**: draw bounding boxes on-the-fly when serving event snapshots ([d95f9ff](https://github.com/rvben/vedetta/commit/d95f9ffd918b5d5ff67110cd417755e6843795a1))
- **ui**: full-screen identification modal for unidentified appearances ([622a447](https://github.com/rvben/vedetta/commit/622a447353446b365940d03ce2e911f30f8f7729))
- **ui**: dismiss/ignore unidentified appearances ([e1dfee5](https://github.com/rvben/vedetta/commit/e1dfee55a3c007e66f33cc8346c32ebb81184c5c))
- **ui**: inline identify picker for unidentified appearances ([aacd31d](https://github.com/rvben/vedetta/commit/aacd31daa99204a01693b914fcccfc013b85ed32))
- **ui**: unidentified appearances section on People page ([1c62426](https://github.com/rvben/vedetta/commit/1c62426d5a101e0f4d5dd51aef71c1e9ba1a0b89))
- **ui**: show face and appearance counts, use best face for thumbnail ([44ecfcc](https://github.com/rvben/vedetta/commit/44ecfccc99816a3a10cf7ba2721400900fac171e))
- **ui**: show faces and appearances when expanding a person ([4e70b83](https://github.com/rvben/vedetta/commit/4e70b83da45fde4dc439f354d8fea3028e75f3cc))
- **ui**: searchable identify picker replaces button list ([ae47ca3](https://github.com/rvben/vedetta/commit/ae47ca319eba70ec5b5ef7c80a05f4ab8da9fa67))
- **ui**: compact face-chip picker replaces button list for identification ([a2eee8d](https://github.com/rvben/vedetta/commit/a2eee8d68f1ea9a2a7000d876e2bfc4227024067))
- **ui**: interactive detection overlay with click-to-identify ([42e2084](https://github.com/rvben/vedetta/commit/42e20841c1b8f3b471cea9a528b8d99f2fa28844))
- assign person events to existing people from event detail ([ad8687b](https://github.com/rvben/vedetta/commit/ad8687bd2fec44b41203ddb0695a9feb899219d2))
- body re-ID fallback and source event linking for person tracking ([efa7a9d](https://github.com/rvben/vedetta/commit/efa7a9d14b0e0d38fb7b70a8b8faf73a48a8c0b9))
- person tracking creates People entries, event detail reloads after tracking ([2644934](https://github.com/rvben/vedetta/commit/2644934a01e336fa5deb38beb566388062f5ad9b))
- **ui**: detection crop preview on event detail for tracking clarity ([5b170b2](https://github.com/rvben/vedetta/commit/5b170b2e40196849db0ab4eb27483c16e2b9e5a9))
- **ui**: real-time doorbell notifications via Server-Sent Events ([06f5c73](https://github.com/rvben/vedetta/commit/06f5c73ea5b49f95e12cd42c79e92b4286d2d08c))
- ONVIF event subscription for native doorbell detection ([3b841dc](https://github.com/rvben/vedetta/commit/3b841dc91c266dbad041ce8ff6dd08de7a74a67e))
- doorbell webhook with face recognition and MQTT snapshots ([5f1ab0a](https://github.com/rvben/vedetta/commit/5f1ab0ae2d8f67311ff0073df023a3edb9b29d95))
- **mqtt**: publish detection snapshots for HA notifications ([29efd83](https://github.com/rvben/vedetta/commit/29efd83ef78f7676fd6c3e9716fcfa6089ce71f8))
- **face**: set sub_label on events when faces match known people ([d0fb9d8](https://github.com/rvben/vedetta/commit/d0fb9d8837c10445d02401526745e8dd6921fe8b))
- per-object threshold, false positive dismissal, and event object filter ([f8c9e4b](https://github.com/rvben/vedetta/commit/f8c9e4bfbf3f692bcd1f8380187efb307985df6f))
- sub-labels, background re-matching, and configurable threshold ([fa71878](https://github.com/rvben/vedetta/commit/fa7187881f5c41301ac1efec8e2850523fb6f2dd))
- **config**: configurable object match threshold ([f6fa077](https://github.com/rvben/vedetta/commit/f6fa077de32cfa5268446c3ff607d9ccef51dc15))
- **ui**: expert-level objects page, input modals, and event filtering ([cb3e827](https://github.com/rvben/vedetta/commit/cb3e8276c1f68bea10ef06fdd14607a1a9ea15f7))
- async object matching, enriched MQTT presence, and HA discovery ([5d67fa3](https://github.com/rvben/vedetta/commit/5d67fa3c456017a5c243dee94c7592f02d006b04))
- MQTT object sightings, presence publishing, and event gallery badges ([2d46278](https://github.com/rvben/vedetta/commit/2d462787007ff10d5489094595f6d9a801cc250b))
- **ui**: add Objects tab to navigation across all pages ([172b2f0](https://github.com/rvben/vedetta/commit/172b2f0c8f6c797289c8f5873be64f4167d8fbfc))
- objects page, multiple references, and improved event tracking UX ([6c929de](https://github.com/rvben/vedetta/commit/6c929de10bde6780da866bf480faf133824f2d93))
- **onnx**: add ReduceMean operator for OSNet model support ([e7e245a](https://github.com/rvben/vedetta/commit/e7e245a0ab60e6493da8d143dc238c070ed189b3))
- object re-identification — track and recognize specific objects ([7194b85](https://github.com/rvben/vedetta/commit/7194b854dded217111a452858e66a11d6a663011))
- **storage**: add known_objects and object_sightings tables ([0e474fe](https://github.com/rvben/vedetta/commit/0e474fec1bc9503a6a77d9f22b6972d2847e15dd))
- **detect**: add OSNet object re-identification embedder ([25889e8](https://github.com/rvben/vedetta/commit/25889e8b535d9680230319547ac4d34dadcb47ff))
- **face**: auto-cluster unmatched faces into unnamed persons ([aa504ef](https://github.com/rvben/vedetta/commit/aa504ef97964c3f2e926268a0dedfe4efdd3aebc))
- **ui**: auto-refresh camera status badges on dashboard ([0e20c22](https://github.com/rvben/vedetta/commit/0e20c2226201245fe2c19c637cd3aae39614631c))
- wire v1 auth and event pipeline into main loop ([010caca](https://github.com/rvben/vedetta/commit/010cacace0bca5c0ef3a58a3544a19507fa08857))
- **recording**: event metadata retention and media availability reconciliation ([84999c7](https://github.com/rvben/vedetta/commit/84999c7ed83ae33e54753f16d02379cfd49820ba))
- **api**: health probes, metrics, face backfill, and polygon zone endpoints ([4a731a8](https://github.com/rvben/vedetta/commit/4a731a86de09d9672e2e2fad458f5a3054c31301))
- **auth**: session auth with CSRF protection and scoped API tokens ([944273e](https://github.com/rvben/vedetta/commit/944273ea6c16d58291cf66f62c4b9b9ac2d654ca))
- **storage**: auth sessions, API tokens, event availability, and zone polygons ([869f469](https://github.com/rvben/vedetta/commit/869f4696f419fcde435931859eb24e72d23ecf26))
- **camera**: polygon zones, detect toggle, degraded status, and event snapshots ([9a47e28](https://github.com/rvben/vedetta/commit/9a47e288dbbfd6971c7b9c3fba3212f9bc00ce03))
- **detect**: remove runtime downloads, fix ONNX ops, and harden decode pipeline ([7f253a8](https://github.com/rvben/vedetta/commit/7f253a8777ac925dcda206a5b1c8b0e30a17a7ed))
- **face**: wire face recognition pipeline into camera loop with people UI ([ad2d8e1](https://github.com/rvben/vedetta/commit/ad2d8e17eaaccac99a62412fd1d4655d182b27cc))
- **face**: SCRFD + MobileFaceNet face detection and embedding pipeline ([d3e78ba](https://github.com/rvben/vedetta/commit/d3e78bae72a994e208eab8ddbd9602a0889eab89))
- **ui**: zone drawing and management on camera page ([3a56b45](https://github.com/rvben/vedetta/commit/3a56b453399403f4ebe653f2d8ab5df35958f0ad))
- zones with presence tracking, timeline thumbnails, ONNX PRelu/Gemm ops ([3153572](https://github.com/rvben/vedetta/commit/3153572cc7856ee21504ae511d39247f520f22fb))
- **ui**: redesign recordings page with per-camera timeline cards ([af18486](https://github.com/rvben/vedetta/commit/af18486bb302757cd8dde0bab1cdc635399ff48a))
- **api**: add POST /api/events/{id}/clip endpoint for clip re-extraction ([b8c803b](https://github.com/rvben/vedetta/commit/b8c803bb27b43e371e7358a353f651ec14990607))
- **api**: recording export endpoint with segment concat and range trim ([5526a4b](https://github.com/rvben/vedetta/commit/5526a4bcd4ae22672bdcae88681f54f20f6860b8))
- **api**: optional HTTPS/TLS support with config validation ([043f2fd](https://github.com/rvben/vedetta/commit/043f2fdf3022402d6fcf6c2f4ee53d4a52941ee2))
- **api**: graceful HTTP shutdown with hardened server timeouts ([3f4edd0](https://github.com/rvben/vedetta/commit/3f4edd0eaf86ea81ded65ca0d26c06805d50bb60))
- **auth**: add HTTP and RTSP authentication with rate limiting ([8cd182b](https://github.com/rvben/vedetta/commit/8cd182b105864456795e53e759e07790bfe16228))
- **stream**: MSE live streaming over WebSocket with fMP4 ([9e9ed8a](https://github.com/rvben/vedetta/commit/9e9ed8afffee1bbad26d600f11d012971c1d29b0))
- **recording**: disk space monitoring with pause/resume and urgent cleanup ([6ffefa7](https://github.com/rvben/vedetta/commit/6ffefa7b4ef339a48b5f799b298d884f858e666f))
- **detect**: auto-download YOLO model from GitHub releases ([51dd9a2](https://github.com/rvben/vedetta/commit/51dd9a26d795789cc772d685d7fa12ac5d1c8b45))
- **stream**: add UDP transport and sub-stream support to RTSP server ([f143d87](https://github.com/rvben/vedetta/commit/f143d87a24d465b8a8166529560f9587e4719499))
- **stream**: add RTSP re-publishing server for Home Assistant integration ([9e89691](https://github.com/rvben/vedetta/commit/9e89691b1702eacd36275fff29a2ac90d74241e4))
- **ui**: camera page stream controls, audio, fullscreen, and stats overlay ([f01ca71](https://github.com/rvben/vedetta/commit/f01ca71952dd6b581e51d72570049ce01d015093))
- **ui**: auto-start WebRTC live stream when opening camera page ([264fac7](https://github.com/rvben/vedetta/commit/264fac7eec03b9e0db2df0168fc9866d4a9f21c8))
- **ui**: show event duration badge in gallery + add tests ([a0468a6](https://github.com/rvben/vedetta/commit/a0468a632a515d27669800d5d3b0b98c9ff1b243))
- **events**: dynamic event duration with max cap ([62999a6](https://github.com/rvben/vedetta/commit/62999a69ded3586ecffc4cfeb1c6b831747714f3))
- **detect**: add configurable label filtering ([5c37efa](https://github.com/rvben/vedetta/commit/5c37efa61e345e0b1468d0a266963388af68a8ae))
- **events**: add recording link and fix clip extraction timing ([27f7cef](https://github.com/rvben/vedetta/commit/27f7cef3dbd08f0abc68957ed344c1e3f5bc0e45))
- **snapshot**: generate annotated event snapshots with bounding boxes ([9e7f481](https://github.com/rvben/vedetta/commit/9e7f481b7061797f06ee2b621c4d23404a68c76a))
- **deploy**: add log rotation configs for macOS and Linux ([fd97075](https://github.com/rvben/vedetta/commit/fd97075b9a57165a9c73cc5834f760c50697f71b))
- **deploy**: add systemd and launchd service files ([e8b73e4](https://github.com/rvben/vedetta/commit/e8b73e4efa6be9689c2937fba27d74def52220aa))
- **media**: auto-download OpenH264 from Cisco on first run ([d0001df](https://github.com/rvben/vedetta/commit/d0001df913c808d7a1a7ceec8f93128d00e6d23a))
- **media**: replace ffmpeg with native Go media pipeline ([c395cf2](https://github.com/rvben/vedetta/commit/c395cf2e0663098f1d70e03aa6acecce26765809))
- **recordings**: add play and download buttons to recording segments ([1880ff7](https://github.com/rvben/vedetta/commit/1880ff71f7d6c7f81e5a0827be4676d8b04d9287))
- **camera**: cache snapshots to disk for offline camera display ([6ec1ff5](https://github.com/rvben/vedetta/commit/6ec1ff5c55ed7d7a9fa4d36d62b6217029c61c1d))
- **ui**: integrate shortcut modal, search, and date picker into all pages ([dfe1da1](https://github.com/rvben/vedetta/commit/dfe1da1e83c7e0b67ceaff59089b16d475f127ea))
- **ui**: add keyboard shortcut modal, timeline hover, infinite scroll, and search ([a01437b](https://github.com/rvben/vedetta/commit/a01437be0a60482aecd1b04577eb901232276c04))
- **ui**: enhance frontend with theme toggle, birdseye, timeline, and playback ([85a2687](https://github.com/rvben/vedetta/commit/85a26875a91283770f5669d095cb588a0a794313))
- **api**: add endpoints for timeline, calendar, events, and playback ([81632b1](https://github.com/rvben/vedetta/commit/81632b16faf1e4be2062161a19346284a80a81e9))
- **storage**: add query methods and fix errcheck warnings ([764445c](https://github.com/rvben/vedetta/commit/764445c95e02fbd4e1f0f009185d8b76ef59d027))
- **api**: add timeline, calendar, event counts, and pagination endpoints ([96d0a3f](https://github.com/rvben/vedetta/commit/96d0a3fbf0784c09780fd642838bd8facc0fc3ec))
- **ui**: complete frontend overhaul with 7 improvements ([154022a](https://github.com/rvben/vedetta/commit/154022a8f8f313f52985176793067c7e028c315b))
- **mqtt**: publish periodic camera status updates ([3e86e4c](https://github.com/rvben/vedetta/commit/3e86e4c9172488cada9842ca0beeb34b0458fd2b))
- **mqtt**: add Home Assistant discovery, availability, and auto-reconnect ([b2f9a54](https://github.com/rvben/vedetta/commit/b2f9a54ca3aae0061341a24664950486cbdcb150))
- **detect**: dual-backend architecture with optional C ONNX Runtime ([675c21e](https://github.com/rvben/vedetta/commit/675c21e17e65b30e9e3f777992f2db0d718bb535))
- **detect**: add pure Go ONNX runtime inference engine ([cf1ad44](https://github.com/rvben/vedetta/commit/cf1ad44ae6e121edf057905455b847c8984f2938))
- **config**: human-readable max_storage setting and remux temp cleanup ([bf30d1a](https://github.com/rvben/vedetta/commit/bf30d1a8d81c1db663d90ae759516ac1cc1fa376))
- **api**: add htmx partial endpoints and wire recorder to server ([86faaad](https://github.com/rvben/vedetta/commit/86faaad374e20e842ae87145c9b0fc60c45c974e))
- **ui**: redesign web dashboard with Control Room Noir theme ([4ba7bd1](https://github.com/rvben/vedetta/commit/4ba7bd1c05a0f2447ccdc453657bb1ec5ff22994))
- **storage**: add data layer queries for UI dashboard ([4899028](https://github.com/rvben/vedetta/commit/4899028d2f78dde1ee63019f2f1db7f0981e64a6))
- complete Phase 1 core features ([3543309](https://github.com/rvben/vedetta/commit/35433091138a90efea3b79793fea8a28cca9b105))
- initial Watchpost scaffold ([8f01cf3](https://github.com/rvben/vedetta/commit/8f01cf3bdc6b9820fe087c4eab8dc4bac12c3e6f))

### Fixed

- **recording**: copy subscribers slice in disk monitor and guard zero interval ([396d89a](https://github.com/rvben/vedetta/commit/396d89ab02aaa40a3c0116d62ada2411afa779dd))
- **ui**: prefer pre-generated MP4 clip over HLS re-segmenter for event playback ([a1395c3](https://github.com/rvben/vedetta/commit/a1395c36301b288949ca392838d74894b9658f0a))
- **ui**: event page video playback uses .m3u8 and hls.js fallback ([9fe9e78](https://github.com/rvben/vedetta/commit/9fe9e78cf2606199cce4a281bb5887cabf6866be))
- **api**: serve event clip inline by default, attachment on ?download=1 ([a2767cf](https://github.com/rvben/vedetta/commit/a2767cf423e7bbe2dee6e2e245b4dc7157fc9c55))
- **ui**: iOS PWA navigation via sw postMessage ([5a011c2](https://github.com/rvben/vedetta/commit/5a011c2f9d8e44851598df651f79f88484950823))
- **notify**: pass raw email to webpush-go, not mailto: URI ([f80a8c0](https://github.com/rvben/vedetta/commit/f80a8c08e86c5b284e2844791a90b28a3c1aae2f))
- **notify**: capture push service response body on non-2xx ([9670dc9](https://github.com/rvben/vedetta/commit/9670dc94b676e780319b7fb964c27e07ba68fc97))
- **vedetta**: use real domain in VAPID subscriber claim ([feb9aac](https://github.com/rvben/vedetta/commit/feb9aac98b4cc09f3010ac7eec7b869bd94ec8aa))
- **storage**: dispatch fanout iterates push_subscriptions, not auth_users ([ad3e26a](https://github.com/rvben/vedetta/commit/ad3e26a0dfbdef4e87bec15761286867db1e8c0a))
- **storage**: drop FK from push_subscriptions to auth_users ([b2daabc](https://github.com/rvben/vedetta/commit/b2daabc572cbc7c67a6f786c49ed21396fe2a754))
- **api**: accept proxy-kind principal on push endpoints ([a46a152](https://github.com/rvben/vedetta/commit/a46a152a36cb4cd1a0f8411d23d792ef230a531b))
- **api**: allow anonymous access to PWA manifest, service worker, and icons ([d3215d7](https://github.com/rvben/vedetta/commit/d3215d7baa9e39ad38e6868ba0aa64d2da248d78))
- **notify**: separate 60s 429 backoff, log timeouts, guard nil vapid, per-sub context ([be33d58](https://github.com/rvben/vedetta/commit/be33d587c3e412165631e9bef4cdaac70baa1e19))
- **onvif**: attach RTSP packet handler after SetupAll ([6a8bd97](https://github.com/rvben/vedetta/commit/6a8bd9748c2e4da3705751f32c5309fd98bc1d98))
- **stream**: honor X-Forwarded-Proto in MSE origin check ([685755d](https://github.com/rvben/vedetta/commit/685755d65cc0bcfcca7eeb49180010306048cbf0))
- **media**: eliminate GC-unsafe C pointer aliasing in transcoder ([997f75d](https://github.com/rvben/vedetta/commit/997f75d44ba306ca7bca9ac6ba2f824452c3866f))
- **recompression**: reset stuck failure counters on startup ([ee77d19](https://github.com/rvben/vedetta/commit/ee77d19f10f9451b87ec9ebd5925c26157ebbe53))
- **retention**: clean orphan segments from removed cameras ([8278c7e](https://github.com/rvben/vedetta/commit/8278c7ee548224ae8fcf624373c5a0abb237e9b7))
- **detect**: handle NaN in fastSigmoid ([75ce3c5](https://github.com/rvben/vedetta/commit/75ce3c5564cd7001b0ff4ded049b6feeb36d40da))
- **ci**: move sgemmThreshold to darwin-only file ([20aeead](https://github.com/rvben/vedetta/commit/20aeeade727a8d398c7c20481c43b4c678dcf169))
- **ci**: upgrade to golangci-lint v2 and resolve all lint findings ([4a28cba](https://github.com/rvben/vedetta/commit/4a28cbaab37b63ef01507f98e1125c6da9a93e8a))
- resolve all golangci-lint v2 findings ([d634722](https://github.com/rvben/vedetta/commit/d63472211f4f121f6cc4cdc0e79125bc0a7d1c8c))
- **onvif**: probe RTSP with credentials when unauthenticated fails ([d80dbdc](https://github.com/rvben/vedetta/commit/d80dbdc4736a8c2a1d8536c0b99266cfb02382e2))
- **api**: use app context for camera start and disconnect RTSP on stop ([5969dda](https://github.com/rvben/vedetta/commit/5969ddad8eb5fda87c5e985051d47315df96aaa4))
- **recording**: recover from OpenH264 panics during recompression ([6a7d322](https://github.com/rvben/vedetta/commit/6a7d32248c8268e43d6c6f15436bc6226b55a37a))
- **recording**: fix flaky export cleanup when context is canceled ([5dbafab](https://github.com/rvben/vedetta/commit/5dbafabad9fe7ea93bf8fee174b2a3ae1c089bcf))
- **media**: fix transcoder failing to decode any frames ([f12d200](https://github.com/rvben/vedetta/commit/f12d2002a7a4f4b550c273f38c5a4600ea7ddffc))
- **media**: serialize OpenH264 C library calls to prevent memory corruption ([b8d3293](https://github.com/rvben/vedetta/commit/b8d32935a1f774c1d253fcfc4c871c68da326eb6))
- **recording**: prevent send on closed channel panic in RecordingConsumer ([262a3d2](https://github.com/rvben/vedetta/commit/262a3d22299a8ae2157b715a0486fb25c0cc1abd))
- **ui**: use 'Single sign-on' label instead of 'reverse proxy' ([301b029](https://github.com/rvben/vedetta/commit/301b0299721a4b70bb5039bb45b0df1ba7712c81))
- **api**: use application context for manual recompression trigger ([678c49b](https://github.com/rvben/vedetta/commit/678c49b2ddf14cbb7d64b54aeff3dfad2c13364e))
- **media**: compute encoder fps from actual sample duration, improve audio track test ([bda68c9](https://github.com/rvben/vedetta/commit/bda68c9ef5b46de7acc2308207e740e554ea6009))
- **storage**: preserve recompression state in SaveSegment upsert; add tests ([27afa37](https://github.com/rvben/vedetta/commit/27afa375af12656640fc464d6d2ef081b78c63ff))
- **playback**: preserve partial fragments from in-progress segments ([45b012c](https://github.com/rvben/vedetta/commit/45b012cdc662a5a343d7a39984dbaf16bdb95263))
- **playback**: skip unreadable segments in HLS playlist generation ([1d8b480](https://github.com/rvben/vedetta/commit/1d8b480b72ce45c04f31ff25cc9733e81f560684))
- **playback**: strip in-band SPS/PPS from HLS segments for Safari compat ([73dcf0e](https://github.com/rvben/vedetta/commit/73dcf0e5185f3963777c16e12b38887a221cbf3c))
- **playback**: serve init segment directly instead of byte-range ([b5d7a2e](https://github.com/rvben/vedetta/commit/b5d7a2eedeb83f196838d5705207f7cbaedb63b4))
- **playback**: split multi-track moofs into single-track for MSE compat ([9c7cb0f](https://github.com/rvben/vedetta/commit/9c7cb0fba9d74c9c5175b3aeb9eb8ab351b710cb))
- **ui**: cap waveform coverage at current time when viewing today ([2c513e3](https://github.com/rvben/vedetta/commit/2c513e3cd7b17e9db159ea6c2ce01365cd5e2291))
- **ui**: render waveform in local time instead of UTC ([c51b764](https://github.com/rvben/vedetta/commit/c51b764d4bf725b7a73875509d577e790f33dc5e))
- **ui**: remove duplicate track variable declaration in scrubTimeline ([7624eb5](https://github.com/rvben/vedetta/commit/7624eb56425c128471356273c8768dfc3ef54be1))
- **ui**: render waveform as distinct bars with gaps instead of continuous line ([ffe2217](https://github.com/rvben/vedetta/commit/ffe2217999ce217fe67bfdb70e73d31196d6583a))
- **ui**: show baseline waveform bars for recorded minutes without motion data ([adf7e2a](https://github.com/rvben/vedetta/commit/adf7e2a6f1bcd4e8e5abb210c9506f82fa55c466))
- **ui**: suppress auto-seek after resume to allow catch-up playback ([b854214](https://github.com/rvben/vedetta/commit/b854214630002dbd20fa7fb50d8035f72988f5ca))
- **ui**: disable auto-seek to live edge while paused ([206b8b8](https://github.com/rvben/vedetta/commit/206b8b8ded2ee1959d6ceb835d8a15446c29a1e8))
- **ui**: seek back to paused position on resume ([d007be6](https://github.com/rvben/vedetta/commit/d007be60111233a74009652da1a50fb5fb1cb56d))
- **ui**: live TV-style pause with seek-to-live button ([bd53429](https://github.com/rvben/vedetta/commit/bd534291923929e899440612acfe25a3fac4e99e))
- **ui**: pause freezes video frame instead of killing stream ([defecb9](https://github.com/rvben/vedetta/commit/defecb9c355bc295bec90bc5d4c14ab7d467e7fb))
- **ptz**: defer context cancel until after reading response body ([fe464c0](https://github.com/rvben/vedetta/commit/fe464c013579deb4cf1c591b85305aff0c28454f))
- **ptz**: try Tapo ONVIF port 2020 first, increase timeout ([2bbd65d](https://github.com/rvben/vedetta/commit/2bbd65dcebcb1c57183a43abe95dc78e0240ffca))
- **ptz**: fallback to WS-Security for GetProfiles ([a76b6e0](https://github.com/rvben/vedetta/commit/a76b6e0b021a3fb0949a456162936951798a27b3))
- **ptz**: probe multiple ONVIF ports for device discovery ([c787c50](https://github.com/rvben/vedetta/commit/c787c50323f8fda018f4bec9884f4c850affd6b2))
- **ui**: hide detection overlay when playing event video ([712d85a](https://github.com/rvben/vedetta/commit/712d85a936f3005396e2462e3baef7f4fa51c878))
- **onboarding**: redirect to login page after setup completes ([056fff1](https://github.com/rvben/vedetta/commit/056fff148a35d7ab1d7e78b50ed9bf49f4867c60))
- ignoring a face in the modal also deletes the face record ([6e394a0](https://github.com/rvben/vedetta/commit/6e394a06784eeaec1ffb790da811a20d7d62d37e))
- **ui**: filter unidentified appearances by score >= 0.65 to reduce false positives ([058e02e](https://github.com/rvben/vedetta/commit/058e02ec60da28f8cb79575afd38bdae0a895c7c))
- **ui**: unwrap events API response for unidentified appearances ([0584d5b](https://github.com/rvben/vedetta/commit/0584d5b4c51b45f6cacc97c99e4adc8b985692cb))
- **api**: include source_event_id in people list response for body-tracked persons ([fde63ef](https://github.com/rvben/vedetta/commit/fde63efa910c6a7e2ca03a305c438d34ce31e961))
- **ui**: preserve original case in 'New: <name>' identify chip ([47450d3](https://github.com/rvben/vedetta/commit/47450d388cc11fe1778a5a8749298f98d4425045))
- save clean snapshots to disk, use annotated only for MQTT display ([8debb57](https://github.com/rvben/vedetta/commit/8debb57d11d70f36f321b54ff2c7181bf876f59b))
- **ui**: show tracked status instead of tracking form on identified events ([6424caf](https://github.com/rvben/vedetta/commit/6424caf03e221c2e36557a534d3efb77dd79838d))
- **detect**: correct pixel indexing for SubImage crops in object embedder ([a4382bb](https://github.com/rvben/vedetta/commit/a4382bb5a2ed1a994322678b09cdeeef04cd963e))
- **ui**: add bottom nav to objects page and link references to source events ([40bf97a](https://github.com/rvben/vedetta/commit/40bf97a9b7ef5c8270102233783b515e94b6d1d1))
- **camera**: scale event bounding box to snapshot resolution ([b48ad7a](https://github.com/rvben/vedetta/commit/b48ad7abc8bd7312097dee3a04d2e8771e6971a0))
- **detect**: fix OSNet model compatibility and add embedding test ([7802f64](https://github.com/rvben/vedetta/commit/7802f64848deef787530e97b80216e0dcb6e7166))
- **face**: save unrecognized faces as unmatched instead of creating persons ([2b23c66](https://github.com/rvben/vedetta/commit/2b23c66de75bb57a382be850749f3b9485a995af))
- **face**: restore correct pixel offset in SCRFD input preprocessing ([f9dd2fa](https://github.com/rvben/vedetta/commit/f9dd2fae7138669bf96e36da109cd87ba45cbd6a))
- **ui**: remove motion state from camera status dot ([65e75bf](https://github.com/rvben/vedetta/commit/65e75bf485b091e88a9d0c4a0db2816876b3ac2e))
- **face**: correct MobileFaceNet download URL and startup race ([f1b6d97](https://github.com/rvben/vedetta/commit/f1b6d9707e23eb474949c31aa9558985637b1036))
- **rtsp-server**: re-packetize H264 RTP to fix oversized packet drops ([eba54e5](https://github.com/rvben/vedetta/commit/eba54e5b2955573f071ac43d18b559f4ed6b083f))
- **ui**: add mobile responsive styles for recordings page ([35b03f5](https://github.com/rvben/vedetta/commit/35b03f5c5ec025b09dade24ac3da5fdc7118220d))
- **ui**: play recording from segment directly instead of downloading clip ([be305d3](https://github.com/rvben/vedetta/commit/be305d3468a5cce35b3b46f4acad1dac482b1986))
- **ui**: show snapshot first on event page, play clip on demand ([88eb003](https://github.com/rvben/vedetta/commit/88eb003e0ea6d3102a16d2c31797bbfca0711c7c))
- **media**: use per-track timescales in fMP4 trim to prevent oversized clips ([0f49918](https://github.com/rvben/vedetta/commit/0f49918522f4bae666e6a119e950787f9b646441))
- **api**: return camera array from /api/cameras so birdseye grid works ([c779bf8](https://github.com/rvben/vedetta/commit/c779bf8376f4c4e6b8ad6a7c2227b6387e9726c5))
- **ui**: order cameras by config file instead of random map iteration ([a5c28ce](https://github.com/rvben/vedetta/commit/a5c28ce25be6a46224bad8f162534236624e4f11))
- **recording**: register segments in DB at creation and hide missing recordings ([e360c53](https://github.com/rvben/vedetta/commit/e360c53281801a98777514aa904be5ad6edbda58))
- **ui**: continuous recording playback across segment boundaries ([84c7375](https://github.com/rvben/vedetta/commit/84c7375182c897be00403d06e8202404305920cd))
- **events**: purge orphaned events with missing snapshot files on startup ([5d6d8cf](https://github.com/rvben/vedetta/commit/5d6d8cf9601be0d06d043ccee215255391d69f6a))
- **api**: fall back to camera snapshot when event snapshot file is missing ([3a30398](https://github.com/rvben/vedetta/commit/3a303981db3ac9b9ed608898974a52d8e96fd365))
- **api**: client-side timezone localization and export handler timeout ([b2b8454](https://github.com/rvben/vedetta/commit/b2b8454e4fdb8d10559a6c248d90f6b2fab994ef))
- **recording**: escalate ensureSegment errors instead of silently dropping them ([abfb52b](https://github.com/rvben/vedetta/commit/abfb52b4d8b0b08d1fabaae9d8545abc3a338c01))
- **recording**: add timeout to export segment processing ([d4a3e28](https://github.com/rvben/vedetta/commit/d4a3e2857d16e0aee4b1647cf7e2a56510a959f8))
- **api**: eliminate health endpoint blocking from SQLite contention and mutex deadlock ([cd860a9](https://github.com/rvben/vedetta/commit/cd860a928944f17ba492dc8dadc9ee016085fbfa))
- **playback**: server-side fMP4 trimming for accurate timeline scrubbing ([c290e25](https://github.com/rvben/vedetta/commit/c290e251fe57a29e91b6fd0ffa84297eab783911))
- **storage**: normalize timestamps to UTC for consistent SQLite comparisons ([d671193](https://github.com/rvben/vedetta/commit/d67119312471e1f78fa308ffd71fcc7c560c3929))
- **webrtc**: reliable live streaming with proper codec negotiation and IPv4 ([acc4c8e](https://github.com/rvben/vedetta/commit/acc4c8efcf3ef9af6da4ee6fc98d5ab5b20f6e81))
- **ui**: recordings page shows no recordings due to timezone and race conditions ([2bb0df6](https://github.com/rvben/vedetta/commit/2bb0df6b84e92ef5dc7937b348d1071c33c1e89b))
- **ui**: mobile bottom tabs not stretching to full width ([bcccb0f](https://github.com/rvben/vedetta/commit/bcccb0fc5fd18662e067a0946273700c8f4a1808))
- **events**: stop emitting track-end events ([a8ea774](https://github.com/rvben/vedetta/commit/a8ea774c22ddda1c5015c3c6fa3d953917ab6691))
- **snapshot**: async decode for main stream, remove debug logging ([db26880](https://github.com/rvben/vedetta/commit/db268804dc0152210d8c39f7e7ffea3392925614))
- **detect**: prepend SPS/PPS for Tapo camera H264 decode ([3681ed6](https://github.com/rvben/vedetta/commit/3681ed69561acdf7dfd6badd91819f67860c22a8))
- **recording**: harden shutdown, startup, and operational observability ([8299391](https://github.com/rvben/vedetta/commit/8299391d8f51193d17028d3bac5856076ebdce48))
- **recording**: graceful shutdown, startup clip race, and operational improvements ([9a3fb6d](https://github.com/rvben/vedetta/commit/9a3fb6de7c0acabe293f35ed538684c7351338c2))
- **camera**: base IsOnline on RTSP connection state instead of decoded frames ([3b6bb86](https://github.com/rvben/vedetta/commit/3b6bb86dd775eaf80aa73b18968e0bdbcf5d84d8))
- **rtsp**: strip credentials from URLs instead of masking them ([244f431](https://github.com/rvben/vedetta/commit/244f4317054ffb0cb79a0303e2d8f19d57bd17fd))
- **rtsp**: redact credentials from RTSP URLs in log output ([262704b](https://github.com/rvben/vedetta/commit/262704b1a8d9089ad0ea79c7133fb84cc81fee17))
- **rtsp**: use single OnPacketRTPAny handler instead of per-media loop ([c6b5440](https://github.com/rvben/vedetta/commit/c6b5440a423086216c15ad984e5666f8ba3447b5))
- **ui**: replace htmx polling with in-place snapshot and stats refresh ([f05e4c7](https://github.com/rvben/vedetta/commit/f05e4c79509161365d0ec23117560b810e7eb08a))
- **detect**: clamp sigmoid LUT index to prevent out-of-bounds panic ([942aa7e](https://github.com/rvben/vedetta/commit/942aa7eacd18da09364d8aa5052eb881fd2115ad))
- **ui**: cap live viewport height so timeline is visible without scrolling ([1cb9241](https://github.com/rvben/vedetta/commit/1cb924162243f8e2784a641c1d87b7b7fdce1193))
- **lint**: resolve all golangci-lint warnings and update .gitignore ([121c68b](https://github.com/rvben/vedetta/commit/121c68bf87d0d96906346b00380f279fd3b28a83))
- **config**: fix parseByteSize suffix ordering and add tests ([06ff8b3](https://github.com/rvben/vedetta/commit/06ff8b3ed2ce129f62a1aeaf5f531cfdf6e4f6f2))
- **api**: use event snapshots in gallery and fix recordings template ([ec63262](https://github.com/rvben/vedetta/commit/ec63262d0e090bb5791b3e42cb6a8fcbd0bf8830))
- **recording**: TOCTOU safety and single-pass clip extraction ([6f9f35a](https://github.com/rvben/vedetta/commit/6f9f35a8ef60df6e8a54b067c2d0970c83e96d67))
- **recording**: fix race condition, retention efficiency, and missing timeouts ([a434f14](https://github.com/rvben/vedetta/commit/a434f1413771fa3f7f4fb0bbfeaa52843a2397dc))
- **detect**: serialize concurrent Detect calls with mutex ([4d41e10](https://github.com/rvben/vedetta/commit/4d41e105bea38ac2a7d8c903cdcaaf9fb3cfbb98))
- **recording**: resolve three bugs preventing segment recording ([61431e0](https://github.com/rvben/vedetta/commit/61431e09118474a6b14ec5feb86c2ee2c1a008fa))

### Performance

- **media**: narrow OpenH264 mutex to create/destroy and encoder ops ([e8d2c19](https://github.com/rvben/vedetta/commit/e8d2c19546e0da3229e983b835163d433bf641a5))
- **recording**: move segment scanning to background goroutine ([6c143ce](https://github.com/rvben/vedetta/commit/6c143cec113da5dcc1528f7cda9c60c9f4c4f2f0))
- **snapshot**: optimize drawing and file I/O ([d428dda](https://github.com/rvben/vedetta/commit/d428ddafbf525f109b074ef4fd1ec778d0e93fdf))
- **stream**: eliminate per-frame allocations in MJPEG handler ([312a765](https://github.com/rvben/vedetta/commit/312a76547efc4c8c8bbc2f64f95d85c3a5df2e49))
- **detect**: eliminate per-frame allocations in motion detector ([2febf47](https://github.com/rvben/vedetta/commit/2febf479072e648b005465fd7f0bc0f85e1736b1))
- **camera**: eliminate per-frame RGBA allocation in frame pipeline ([6096041](https://github.com/rvben/vedetta/commit/60960419fbac9238b4b21c03a15e48d148cfa26d))
- **detect**: reuse preprocessing buffer across inference calls ([ce577e0](https://github.com/rvben/vedetta/commit/ce577e023bd9fffbd28a6d32cb090e0890695b9a))
- **detect**: optimize ONNX runtime from 97ms to 72ms per inference ([d45baf8](https://github.com/rvben/vedetta/commit/d45baf8ddd4fca1bd9dfe3841a6e5659db5c4caf))
- **detect**: optimize pure Go ONNX runtime from 533ms to 97ms per inference ([c9774c8](https://github.com/rvben/vedetta/commit/c9774c83e64ca99780778c7d9c5c02fe30366b46))
- **recording**: optimize continuous recording storage ([b3807a6](https://github.com/rvben/vedetta/commit/b3807a69c4cbd0048c9182b9e27e65b27b3285bb))
