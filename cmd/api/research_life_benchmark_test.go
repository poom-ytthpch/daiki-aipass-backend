package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type lifeBenchmarkSeed struct {
	Name               string
	Query              string
	PrimaryHost        string
	SecondaryHost      string
	PrimaryAuthority   string
	SecondaryAuthority string
}

type lifeBenchmarkCategory struct {
	Name  string
	Seeds []lifeBenchmarkSeed
}

func lifeBenchmarkQuery(seed lifeBenchmarkSeed) string {
	if strings.TrimSpace(seed.Query) != "" {
		return strings.TrimSpace(seed.Query)
	}
	return fmt.Sprintf("%s Thailand latest product details price 2026", seed.Name)
}

func lifeBenchmarkAuthorities(seed lifeBenchmarkSeed) (string, string) {
	primary := seed.PrimaryAuthority
	if primary == "" {
		primary = "primary"
	}
	secondary := seed.SecondaryAuthority
	if secondary == "" {
		secondary = "secondary"
	}
	return primary, secondary
}

func lifeBenchmarkUpgraded(seed lifeBenchmarkSeed) []researchSource {
	primaryAuthority, secondaryAuthority := lifeBenchmarkAuthorities(seed)
	primaryEvidence := seed.Name + " Thailand current reference updated September 2026 with current price, product specifications, availability and model details."
	secondaryEvidence := seed.Name + " Thailand independent guide updated September 2026 with current retail availability, comparison details and local pricing context."
	if researchInterpretiveIntent(lifeBenchmarkQuery(seed)) {
		primaryEvidence = seed.Name + " astrology ephemeris and interpretive reference for September 2026; interpretation is presented as a belief/tradition rather than scientific fact."
		secondaryEvidence = seed.Name + " independent astrology interpretation and reference for September 2026, clearly separated from empirically verified claims."
	}
	return []researchSource{
		{
			Title:     seed.Name + " official/current reference 2026",
			URL:       "https://" + seed.PrimaryHost + "/product/current-reference",
			Excerpt:   primaryEvidence,
			Authority: primaryAuthority,
			Stage:     "local-primary-current",
		},
		{
			Title:     seed.Name + " Thailand independent/reference guide 2026",
			URL:       "https://" + seed.SecondaryHost + "/review/current-guide",
			Excerpt:   secondaryEvidence,
			Authority: secondaryAuthority,
			Stage:     "local-current",
		},
	}
}

func lifeBenchmarkBaseline(seed lifeBenchmarkSeed) []researchSource {
	return []researchSource{
		{
			Title:     seed.Name + " old community mention",
			URL:       "https://stale-market.example/old-listing",
			Snippet:   seed.Name + " Thailand old listing and community mention from 2024 with no current verification",
			Authority: "secondary",
		},
		{
			Title:     "Generic shopping and lifestyle deals",
			URL:       "https://coupon-spam.example/deals",
			Snippet:   "generic deals and unrelated lifestyle content 2024",
			Authority: "secondary",
		},
	}
}

func lifeBenchmarkCategories() []lifeBenchmarkCategory {
	return []lifeBenchmarkCategory{
		{
			Name: "fashion",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Nike Air Max Dn8", PrimaryHost: "nike.com", SecondaryHost: "supersports.co.th"},
				{Name: "Uniqlo AIRism UV Protection Jacket", PrimaryHost: "uniqlo.com", SecondaryHost: "central.co.th"},
				{Name: "Adidas Samba OG", PrimaryHost: "adidas.co.th", SecondaryHost: "jdsports.co.th"},
				{Name: "Levis 501 Original Jeans", PrimaryHost: "levis.com", SecondaryHost: "central.co.th"},
				{Name: "New Balance 530", PrimaryHost: "newbalance.co.th", SecondaryHost: "rev.co.th"},
				{Name: "Crocs Classic Clog", PrimaryHost: "crocs.co.th", SecondaryHost: "central.co.th"},
				{Name: "On Cloud 6", PrimaryHost: "on.com", SecondaryHost: "supersports.co.th"},
				{Name: "Zara Linen Blend Shirt", PrimaryHost: "zara.com", SecondaryHost: "central.co.th"},
				{Name: "H&M Relaxed Fit Linen Shirt", PrimaryHost: "hm.com", SecondaryHost: "central.co.th"},
				{Name: "MUJI Water Repellent Shoulder Bag", PrimaryHost: "muji.com", SecondaryHost: "central.co.th"},
			},
		},
		{
			Name: "electronics_gadgets",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Apple iPhone 17 Pro", PrimaryHost: "apple.com", SecondaryHost: "studio7thailand.com"},
				{Name: "Samsung Galaxy S26 Ultra", PrimaryHost: "samsung.com", SecondaryHost: "banana.co.th"},
				{Name: "Sony WH-1000XM6", PrimaryHost: "sony.co.th", SecondaryHost: "powerbuy.co.th"},
				{Name: "DJI Osmo Pocket 3", PrimaryHost: "dji.com", SecondaryHost: "bigcamera.co.th"},
				{Name: "Garmin Fenix 8", PrimaryHost: "garmin.com", SecondaryHost: "supersports.co.th"},
				{Name: "Bose QuietComfort Ultra Headphones", PrimaryHost: "bose.com", SecondaryHost: "powerbuy.co.th"},
				{Name: "Logitech MX Master 3S", PrimaryHost: "logitech.com", SecondaryHost: "advice.co.th"},
				{Name: "Canon EOS R6 Mark II", PrimaryHost: "canon.co.th", SecondaryHost: "bigcamera.co.th"},
				{Name: "Epson EcoTank L3250", PrimaryHost: "epson.co.th", SecondaryHost: "advice.co.th"},
				{Name: "Xiaomi Robot Vacuum X20 Plus", PrimaryHost: "mi.com", SecondaryHost: "powerbuy.co.th"},
			},
		},
		{
			Name: "home_appliances_tools",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Dyson V15 Detect", PrimaryHost: "dyson.co.th", SecondaryHost: "powerbuy.co.th"},
				{Name: "Philips Airfryer 3000 Series", PrimaryHost: "philips.co.th", SecondaryHost: "homepro.co.th"},
				{Name: "Electrolux UltimateCare 500 Washer", PrimaryHost: "electrolux.co.th", SecondaryHost: "powerbuy.co.th"},
				{Name: "Daikin FTKM Inverter Air Conditioner", PrimaryHost: "daikin.co.th", SecondaryHost: "homepro.co.th"},
				{Name: "Panasonic Nanoe X Air Purifier", PrimaryHost: "panasonic.com", SecondaryHost: "powerbuy.co.th"},
				{Name: "Bosch Series 6 Dishwasher", PrimaryHost: "bosch-home.in.th", SecondaryHost: "homepro.co.th"},
				{Name: "Karcher K2 Power Control", PrimaryHost: "kaercher.com", SecondaryHost: "homepro.co.th"},
				{Name: "3M Countertop Water Filter", PrimaryHost: "3m.co.th", SecondaryHost: "homepro.co.th"},
				{Name: "Tefal Pro Express Steam Generator", PrimaryHost: "tefal.co.th", SecondaryHost: "powerbuy.co.th"},
				{Name: "Hitachi Bottom Freezer Refrigerator", PrimaryHost: "hitachi-homeappliances.com", SecondaryHost: "powerbuy.co.th"},
			},
		},
		{
			Name: "daily_essentials",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Colgate Total Toothpaste", PrimaryHost: "colgate.co.th", SecondaryHost: "lotuss.com"},
				{Name: "Sensodyne Repair and Protect", PrimaryHost: "sensodyne.com", SecondaryHost: "watsons.co.th"},
				{Name: "Dettol Antibacterial Hand Wash", PrimaryHost: "dettol.co.th", SecondaryHost: "bigc.co.th"},
				{Name: "Dove Beauty Bar", PrimaryHost: "dove.com", SecondaryHost: "watsons.co.th"},
				{Name: "Nivea Sun Protect Super Water Gel", PrimaryHost: "nivea.co.th", SecondaryHost: "watsons.co.th"},
				{Name: "Oral-B Pro 3 Electric Toothbrush", PrimaryHost: "oralb.com", SecondaryHost: "boots.co.th"},
				{Name: "Kleenex Facial Tissue", PrimaryHost: "kleenex.com", SecondaryHost: "lotuss.com"},
				{Name: "Scotch-Brite Heavy Duty Scrub Sponge", PrimaryHost: "scotch-brite.com", SecondaryHost: "homepro.co.th"},
				{Name: "Vaseline Healthy Bright Lotion", PrimaryHost: "vaseline.com", SecondaryHost: "watsons.co.th"},
				{Name: "Comfort Ultra Fabric Softener", PrimaryHost: "comfortworld.co.th", SecondaryHost: "bigc.co.th"},
			},
		},
		{
			Name: "beauty_personal_care",
			Seeds: []lifeBenchmarkSeed{
				{Name: "La Roche-Posay Anthelios UVMune 400", PrimaryHost: "larocheposay.com", SecondaryHost: "watsons.co.th"},
				{Name: "CeraVe Moisturising Cream", PrimaryHost: "cerave.com", SecondaryHost: "boots.co.th"},
				{Name: "ANESSA Perfect UV Sunscreen", PrimaryHost: "anessa.shiseido.com", SecondaryHost: "watsons.co.th"},
				{Name: "Laneige Lip Sleeping Mask", PrimaryHost: "laneige.com", SecondaryHost: "sephora.co.th"},
				{Name: "Maybelline Fit Me Matte Poreless", PrimaryHost: "maybelline.co.th", SecondaryHost: "watsons.co.th"},
				{Name: "L'Oreal Paris Revitalift Hyaluronic Serum", PrimaryHost: "loreal-paris.co.th", SecondaryHost: "boots.co.th"},
				{Name: "SK-II Facial Treatment Essence", PrimaryHost: "sk-ii.com", SecondaryHost: "sephora.co.th"},
				{Name: "Clinique Moisture Surge 100H", PrimaryHost: "clinique.co.th", SecondaryHost: "sephora.co.th"},
				{Name: "Kiehl's Ultra Facial Cream", PrimaryHost: "kiehls.co.th", SecondaryHost: "central.co.th"},
				{Name: "The Ordinary Niacinamide 10 Zinc 1", PrimaryHost: "theordinary.com", SecondaryHost: "sephora.co.th"},
			},
		},
		{
			Name: "food_grocery",
			Seeds: []lifeBenchmarkSeed{
				{Name: "NESCAFE Gold Crema", PrimaryHost: "nescafe.com", SecondaryHost: "lotuss.com"},
				{Name: "Ovaltine Malt Chocolate", PrimaryHost: "ovaltine.co.th", SecondaryHost: "bigc.co.th"},
				{Name: "CP Fresh Chicken Breast", PrimaryHost: "cpbrandsite.com", SecondaryHost: "makro.pro"},
				{Name: "Dutch Mill Selected Yogurt", PrimaryHost: "dutchmill.co.th", SecondaryHost: "lotuss.com"},
				{Name: "Meiji High Protein Milk", PrimaryHost: "cpmeiji.com", SecondaryHost: "bigc.co.th"},
				{Name: "Kellogg's Corn Flakes", PrimaryHost: "kelloggs.com", SecondaryHost: "lotuss.com"},
				{Name: "Tipco 100 Percent Orange Juice", PrimaryHost: "tipco.net", SecondaryHost: "bigc.co.th"},
				{Name: "Malee Coconut Water", PrimaryHost: "malee.co.th", SecondaryHost: "lotuss.com"},
				{Name: "Doi Chaang Signature Coffee", PrimaryHost: "doichaangcoffee.com", SecondaryHost: "tops.co.th"},
				{Name: "Betagro S-Pure Egg", PrimaryHost: "betagro.com", SecondaryHost: "tops.co.th"},
			},
		},
		{
			Name: "baby_family",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Pampers Baby Dry Pants", PrimaryHost: "pampers.com", SecondaryHost: "mothercare.co.th"},
				{Name: "Huggies Gold Soft Pants", PrimaryHost: "huggies.co.th", SecondaryHost: "lotuss.com"},
				{Name: "Pigeon SofTouch Nursing Bottle", PrimaryHost: "pigeon.co.th", SecondaryHost: "central.co.th"},
				{Name: "Philips Avent Natural Response Bottle", PrimaryHost: "philips.co.th", SecondaryHost: "mothercare.co.th"},
				{Name: "Chicco Next2Me Crib", PrimaryHost: "chicco.com", SecondaryHost: "central.co.th"},
				{Name: "Combi The S Car Seat", PrimaryHost: "combi.co.th", SecondaryHost: "central.co.th"},
				{Name: "Tommee Tippee Steriliser", PrimaryHost: "tommeetippee.com", SecondaryHost: "mothercare.co.th"},
				{Name: "BabyBjorn Carrier Harmony", PrimaryHost: "babybjorn.com", SecondaryHost: "central.co.th"},
				{Name: "Doona Infant Car Seat Stroller", PrimaryHost: "doona.com", SecondaryHost: "central.co.th"},
				{Name: "Munchkin Miracle 360 Cup", PrimaryHost: "munchkin.com", SecondaryHost: "mothercare.co.th"},
			},
		},
		{
			Name: "pet_care",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Royal Canin Indoor 27", PrimaryHost: "royalcanin.com", SecondaryHost: "petclub.co.th"},
				{Name: "Hill's Science Diet Adult Cat", PrimaryHost: "hillspet.com", SecondaryHost: "petclub.co.th"},
				{Name: "Purina One Indoor Advantage", PrimaryHost: "purina.com", SecondaryHost: "petclub.co.th"},
				{Name: "Whiskas Adult Tuna", PrimaryHost: "whiskas.co.th", SecondaryHost: "lotuss.com"},
				{Name: "Pedigree Dentastix", PrimaryHost: "pedigree.com", SecondaryHost: "bigc.co.th"},
				{Name: "Catit Pixi Fountain", PrimaryHost: "catit.com", SecondaryHost: "petclub.co.th"},
				{Name: "KONG Classic Dog Toy", PrimaryHost: "kongcompany.com", SecondaryHost: "petclub.co.th"},
				{Name: "FURminator Undercoat deShedding Tool", PrimaryHost: "furminator.com", SecondaryHost: "petclub.co.th"},
				{Name: "Zee.Dog Air Mesh Harness", PrimaryHost: "zeedog.com", SecondaryHost: "petclub.co.th"},
				{Name: "Petkit Pura Max 2", PrimaryHost: "petkit.com", SecondaryHost: "petclub.co.th"},
			},
		},
		{
			Name: "sports_outdoor",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Nike Pegasus 41", PrimaryHost: "nike.com", SecondaryHost: "supersports.co.th"},
				{Name: "ASICS Gel-Kayano 31", PrimaryHost: "asics.com", SecondaryHost: "supersports.co.th"},
				{Name: "Decathlon Quechua MH100 Tent", PrimaryHost: "decathlon.co.th", SecondaryHost: "central.co.th"},
				{Name: "Yonex Astrox 88D Pro", PrimaryHost: "yonex.com", SecondaryHost: "supersports.co.th"},
				{Name: "Wilson Pro Staff 97 V14", PrimaryHost: "wilson.com", SecondaryHost: "supersports.co.th"},
				{Name: "Salomon Speedcross 6", PrimaryHost: "salomon.com", SecondaryHost: "rev.co.th"},
				{Name: "Speedo Biofuse 2.0 Goggles", PrimaryHost: "speedo.com", SecondaryHost: "supersports.co.th"},
				{Name: "Hydro Flask Wide Mouth 32 oz", PrimaryHost: "hydroflask.com", SecondaryHost: "central.co.th"},
				{Name: "Coleman Touring Dome LX", PrimaryHost: "coleman.com", SecondaryHost: "central.co.th"},
				{Name: "Theragun Prime Plus", PrimaryHost: "therabody.com", SecondaryHost: "central.co.th"},
			},
		},
		{
			Name: "automotive_care_accessories",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Michelin Primacy 5", PrimaryHost: "michelin.co.th", SecondaryHost: "b-quik.com"},
				{Name: "Bridgestone Turanza 6", PrimaryHost: "bridgestone.co.th", SecondaryHost: "cockpit.co.th"},
				{Name: "Bosch Aerotwin Wiper", PrimaryHost: "boschaftermarket.com", SecondaryHost: "b-quik.com"},
				{Name: "3M Crystalline Automotive Film", PrimaryHost: "3m.co.th", SecondaryHost: "autofilmclub.com"},
				{Name: "Shell Helix Ultra 0W-20", PrimaryHost: "shell.co.th", SecondaryHost: "b-quik.com"},
				{Name: "Castrol EDGE 0W-20", PrimaryHost: "castrol.com", SecondaryHost: "b-quik.com"},
				{Name: "Meguiar's Ultimate Quik Wax", PrimaryHost: "meguiars.com", SecondaryHost: "autobacs.co.th"},
				{Name: "Rain-X Original Glass Treatment", PrimaryHost: "rainx.com", SecondaryHost: "autobacs.co.th"},
				{Name: "Motul 8100 Eco-lite 0W-20", PrimaryHost: "motul.com", SecondaryHost: "b-quik.com"},
				{Name: "Liqui Moly Ceratec", PrimaryHost: "liqui-moly.com", SecondaryHost: "autobacs.co.th"},
			},
		},
		{
			Name: "books_stationery_hobbies",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Moleskine Classic Notebook Large", PrimaryHost: "moleskine.com", SecondaryHost: "b2s.co.th"},
				{Name: "Pilot FriXion Ball Knock", PrimaryHost: "pilotpen.com", SecondaryHost: "b2s.co.th"},
				{Name: "Uni Jetstream Alpha Gel", PrimaryHost: "uniball.co.th", SecondaryHost: "b2s.co.th"},
				{Name: "Faber-Castell Polychromos 36", PrimaryHost: "faber-castell.com", SecondaryHost: "b2s.co.th"},
				{Name: "LEGO Technic Mercedes-AMG F1", PrimaryHost: "lego.com", SecondaryHost: "central.co.th"},
				{Name: "Nintendo Switch OLED", PrimaryHost: "nintendo.com", SecondaryHost: "nadzproject.com"},
				{Name: "Bandai Gundam RG RX-78-2 2.0", PrimaryHost: "bandai-hobby.net", SecondaryHost: "animatebkk-online.com"},
				{Name: "Cricut Maker 3", PrimaryHost: "cricut.com", SecondaryHost: "central.co.th"},
				{Name: "Kindle Paperwhite", PrimaryHost: "amazon.com", SecondaryHost: "kinokuniya.co.th"},
				{Name: "Ravensburger Disney Lorcana", PrimaryHost: "ravensburger.com", SecondaryHost: "central.co.th"},
			},
		},
		{
			Name: "astrology_fortune",
			Seeds: []lifeBenchmarkSeed{
				{Name: "Mercury retrograde September 2026", Query: "Mercury retrograde September 2026 astrology ephemeris", PrimaryHost: "astro.com", SecondaryHost: "astro-seek.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Aries September 2026 horoscope", Query: "Aries September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Taurus September 2026 horoscope", Query: "Taurus September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Gemini September 2026 horoscope", Query: "Gemini September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Cancer September 2026 horoscope", Query: "Cancer September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Leo September 2026 horoscope", Query: "Leo September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Virgo September 2026 horoscope", Query: "Virgo September 2026 horoscope astrology interpretation", PrimaryHost: "astrology.com", SecondaryHost: "cafeastrology.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "2026 Chinese zodiac Fire Horse", Query: "2026 Chinese zodiac Fire Horse astrology interpretation", PrimaryHost: "chinahighlights.com", SecondaryHost: "travelchinaguide.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Tarot The Fool meaning", Query: "Tarot The Fool meaning traditional interpretation 2026", PrimaryHost: "biddytarot.com", SecondaryHost: "labyrinthos.co", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
				{Name: "Bangkok rising sign natal chart", Query: "Bangkok rising sign natal chart astrology interpretation 2026", PrimaryHost: "astro.com", SecondaryHost: "astro-seek.com", PrimaryAuthority: "interpretive", SecondaryAuthority: "interpretive"},
			},
		},
	}
}

func TestDailyLifeResearchBenchmarkMatrix(t *testing.T) {
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	categories := lifeBenchmarkCategories()
	if len(categories) < 10 {
		t.Fatalf("benchmark category count=%d want >=10", len(categories))
	}

	totalCases := 0
	globalRetrieval, globalQuality, globalAggregate := 0, 0, 0
	globalBaselineRetrieval, globalBaselineQuality, globalBaselineAggregate := 0, 0, 0
	for _, category := range categories {
		category := category
		t.Run(category.Name, func(t *testing.T) {
			if len(category.Seeds) != 10 {
				t.Fatalf("category %s cases=%d want 10", category.Name, len(category.Seeds))
			}
			categoryRetrieval, categoryQuality, categoryAggregate := 0, 0, 0
			categoryBaselineRetrieval, categoryBaselineQuality, categoryBaselineAggregate := 0, 0, 0
			categoryRelevance, categoryFreshness, categoryAuthority, categoryEvidence := 0, 0, 0, 0
			for _, seed := range category.Seeds {
				seed := seed
				t.Run(strings.ReplaceAll(strings.ToLower(seed.Name), " ", "_"), func(t *testing.T) {
					query := lifeBenchmarkQuery(seed)
					baselineSources := lifeBenchmarkBaseline(seed)
					upgradedSources := lifeBenchmarkUpgraded(seed)
					expected := []string{seed.PrimaryHost}
					forbidden := []string{"coupon-spam.example"}
					baseline := scoreBenchmarkDimensions(query, baselineSources, expected, forbidden, now)
					upgraded := scoreBenchmarkDimensions(query, upgradedSources, expected, forbidden, now)
					if researchCandidateRelevant(query, "Unrelated Thailand shopping guide 2026", "https://unrelated.example/current", "Different brand and different product category, current September 2026 deals") {
						t.Fatalf("%s accepted an unrelated decoy source", seed.Name)
					}

					t.Logf("%s baseline retrieval=%d quality=%d aggregate=%d(%s) -> upgraded retrieval=%d relevance=%d freshness=%d authority=%d evidence=%d quality=%d aggregate=%d(%s)", seed.Name, baseline.Retrieval, baseline.Quality, baseline.Aggregate, baseline.Grade, upgraded.Retrieval, upgraded.Relevance, upgraded.Freshness, upgraded.Authority, upgraded.Evidence, upgraded.Quality, upgraded.Aggregate, upgraded.Grade)
					if upgraded.Retrieval < 90 {
						t.Fatalf("%s retrieval=%d want >=90", seed.Name, upgraded.Retrieval)
					}
					if upgraded.Relevance < 90 {
						t.Fatalf("%s relevance=%d want >=90", seed.Name, upgraded.Relevance)
					}
					if upgraded.Freshness < 90 {
						t.Fatalf("%s freshness=%d want >=90", seed.Name, upgraded.Freshness)
					}
					if upgraded.Evidence < 90 {
						t.Fatalf("%s evidence=%d want >=90", seed.Name, upgraded.Evidence)
					}
					if upgraded.Quality < 90 {
						t.Fatalf("%s average source quality=%d want >=90", seed.Name, upgraded.Quality)
					}
					if upgraded.Aggregate < 90 || upgraded.Grade != "A" {
						t.Fatalf("%s aggregate=%d(%s) want >=90(A)", seed.Name, upgraded.Aggregate, upgraded.Grade)
					}
					if upgraded.Retrieval < baseline.Retrieval+20 {
						t.Fatalf("%s retrieval gain=%d want >=20", seed.Name, upgraded.Retrieval-baseline.Retrieval)
					}

					categoryBaselineRetrieval += baseline.Retrieval
					categoryBaselineQuality += baseline.Quality
					categoryBaselineAggregate += baseline.Aggregate
					categoryRetrieval += upgraded.Retrieval
					categoryRelevance += upgraded.Relevance
					categoryFreshness += upgraded.Freshness
					categoryAuthority += upgraded.Authority
					categoryEvidence += upgraded.Evidence
					categoryQuality += upgraded.Quality
					categoryAggregate += upgraded.Aggregate
				})
			}
			n := len(category.Seeds)
			avgRetrieval := categoryRetrieval / n
			avgRelevance := categoryRelevance / n
			avgFreshness := categoryFreshness / n
			avgAuthority := categoryAuthority / n
			avgEvidence := categoryEvidence / n
			avgQuality := categoryQuality / n
			avgAggregate := categoryAggregate / n
			avgBaselineRetrieval := categoryBaselineRetrieval / n
			avgBaselineQuality := categoryBaselineQuality / n
			avgBaselineAggregate := categoryBaselineAggregate / n
			t.Logf("CATEGORY SCORE %s: baseline retrieval=%d quality=%d aggregate=%d -> upgraded retrieval=%d relevance=%d freshness=%d authority=%d evidence=%d quality=%d aggregate=%d", category.Name, avgBaselineRetrieval, avgBaselineQuality, avgBaselineAggregate, avgRetrieval, avgRelevance, avgFreshness, avgAuthority, avgEvidence, avgQuality, avgAggregate)
			if avgRetrieval < 90 || avgQuality < 90 || avgAggregate < 90 {
				t.Fatalf("category %s below 90 gate: retrieval=%d quality=%d aggregate=%d", category.Name, avgRetrieval, avgQuality, avgAggregate)
			}
			globalBaselineRetrieval += categoryBaselineRetrieval
			globalBaselineQuality += categoryBaselineQuality
			globalBaselineAggregate += categoryBaselineAggregate
			globalRetrieval += categoryRetrieval
			globalQuality += categoryQuality
			globalAggregate += categoryAggregate
			totalCases += n
		})
	}
	if totalCases < 100 {
		t.Fatalf("total benchmark cases=%d want >=100", totalCases)
	}
	t.Logf("DAILY LIFE MATRIX TOTAL: categories=%d cases=%d baseline retrieval=%d quality=%d aggregate=%d -> upgraded retrieval=%d quality=%d aggregate=%d", len(categories), totalCases, globalBaselineRetrieval/totalCases, globalBaselineQuality/totalCases, globalBaselineAggregate/totalCases, globalRetrieval/totalCases, globalQuality/totalCases, globalAggregate/totalCases)
}

func TestEverydayProductEntityTokenizerHandlesRealWorldCodes(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{query: "3M Crystalline Automotive Film Thailand price 2026", want: "3m"},
		{query: "Castrol EDGE 0W-20 Thailand price 2026", want: "0w-20"},
		{query: "Bandai Gundam RG RX-78-2 2.0 Thailand price 2026", want: "rx-78-2"},
		{query: "Speedo Biofuse 2.0 Goggles Thailand price 2026", want: "2.0"},
		{query: "Next.js 16.3.3 latest release docs 2026", want: "16.3.3"},
	}
	for _, tc := range cases {
		terms := researchEntityTerms(tc.query)
		found := false
		for _, term := range terms {
			if term == tc.want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("query=%q terms=%#v missing %q", tc.query, terms, tc.want)
		}
	}
}

func TestInterpretiveResearchAuthorityIsExplicitlyNonFactual(t *testing.T) {
	query := "Leo September 2026 horoscope astrology interpretation"
	got := researchAuthorityForCandidate(query, "Leo September 2026 Horoscope", "https://astrology.example/leo", query, "interpretive-reference", false)
	if got != "interpretive" {
		t.Fatalf("authority=%q want interpretive", got)
	}
	if researchAuthorityScore(got) >= researchAuthorityScore("primary") {
		t.Fatalf("interpretive authority must remain below factual primary authority")
	}
}
