# Generates a synthetic sermon benchmark set: text -> WAV via Windows SAPI,
# plus manifest.json with gold references so asr-bench can score the set.
#
# This produces CLEAN synthetic audio (no room reverb, no PA distortion), so
# treat results as optimistic upper bounds. It exists to vary the spoken
# REFERENCE PHRASINGS, which LibriVox-style Bible reading audio never
# contains. Benchmark real sanctuary recordings before picking the engine.
#
# The set covers all four detection lanes: explicit references, contextual
# bare-verse calls (with an "established" passage on the wall), word-for-word
# quotations, and reference corrections. Each clip declares the type it
# exercises so a lane only scores its own hearings.
#
# Usage:  powershell -File benchmarks/make-tts-set.ps1
# Output: benchmarks/tts-set/ (audio and manifest are committed; the real
# recordings in sermon-set stay gitignored on the machine that made them)

$ErrorActionPreference = "Stop"
$setDir = "benchmarks\tts-set"
New-Item -ItemType Directory -Force $setDir | Out-Null

Add-Type -AssemblyName System.Speech
$synth = [System.Speech.Synthesis.SpeechSynthesizer]::new()

# Vary rate/voice between segments so no two clips sound like test data.
$voiceNames = @($synth.GetInstalledVoices() | ForEach-Object { $_.VoiceInfo.Name })

# Bake rate/voice variety into segments so no two clips sound like test data.
$segments = @(
    @{
        id    = "opening-romans"
        rate  = -2
        refs  = @("Romans 8:28")
        text  = "Good morning, everyone. Let us open our Bibles this morning to Romans, chapter eight, verse twenty eight. And we know that all things work together for good to them that love God. Let us pray."
    },
    @{
        id    = "second-corinthians"
        rate  = 1
        refs  = @("2 Corinthians 4")
        text  = "This morning we continue in second Corinthians, chapter four. In the middle of trouble we do not lose heart. Our light affliction works a far more eternal weight of glory."
    },
    @{
        id    = "gospel-psalm-combo"
        rate  = 0
        refs  = @("John 3:16", "Psalms 51:10")
        text  = "Please turn to John, three sixteen. And then hold your place and go over to Psalm fifty one, verse ten, and we will come back to the gospel in a moment."
    },
    @{
        id    = "matthew-range"
        rate  = 2
        refs  = @("Matthew 11:28-30")
        text  = "The passage for tonight is Matthew, chapter eleven, verses twenty eight through thirty. Come unto me, all ye that labour and are heavy laden, and I will give you rest."
    },
    @{
        id    = "announcements-trap"
        rate  = -1
        refs  = @()
        text  = "Before we open the word, a few announcements. The youth group meets on the twenty eighth at six thirty. The choir still needs fifteen volunteers for the Christmas program, and the food pantry served one hundred nineteen families last month."
    },
    @{
        id    = "revelation-bookends"
        rate  = 0
        refs  = @("Genesis 1:1", "Revelation 22:21")
        text  = "Genesis one one opens the whole story: in the beginning God created the heaven and the earth. And Revelation, twenty two twenty one, closes it: the grace of our Lord Jesus Christ be with you all."
    },
    @{
        id    = "peter-psalm-hundred"
        rate  = 1
        refs  = @("1 Peter 5:7", "Psalms 119:105")
        text  = "Two texts finish our series tonight. First Peter, five seven: casting all your care upon him, for he careth for you. And Psalm one hundred nineteen, verse one hundred five: thy word is a lamp unto my feet."
    },
    @{
        id    = "the-book-of-james"
        rate  = -3
        refs  = @("James 1:5")
        text  = "Turn with me, if you would, to the book of James. Chapter one. Verse five. If any of you lack wisdom, let him ask of God, that giveth to all men liberally."
    },
    @{
        id    = "samuel-anniversary"
        rate  = 2
        refs  = @("1 Samuel 1")
        text  = "For our tenth anniversary we return to where it all started, first Samuel chapter one, and Hannah's prayer. Also remember today is May twenty eighth at one hundred nineteen years our oldest member turns one hundred twenty."
    },

    # Contextual lane: a bare "verse N" call against the passage on the wall.
    @{
        id          = "verse-call-john"
        type        = "contextual"
        established = "John 3"
        rate        = 0
        refs        = @("John 3:29")
        text        = "Now read verse twenty nine with me. That is where the promise lands tonight, and it is the verse the whole chapter has been walking toward."
    },
    @{
        id          = "verse-call-psalm"
        type        = "contextual"
        established = "Psalms 23"
        rate        = -1
        refs        = @("Psalms 23:4")
        text        = "Let us go back to verse four for a moment. That is the verse that carries the whole psalm, so read it slowly and let it settle."
    },
    @{
        id          = "verse-call-trap"
        type        = "contextual"
        established = "Romans 8"
        rate        = 1
        refs        = @()
        text        = "The youth group meets on the twenty eighth at six thirty, and the choir still needs fifteen volunteers for the Christmas program."
    },

    # Quote lane: scripture read word for word without naming the passage.
    @{
        id    = "shepherd-quote"
        type  = "quote"
        rate  = -2
        refs  = @("Psalms 23:1-2")
        text  = "The Lord is my shepherd; I shall not want. He maketh me to lie down in green pastures: he leadeth me beside the still waters."
    },
    @{
        id    = "john316-quote"
        type  = "quote"
        rate  = 0
        refs  = @("John 3:16")
        text  = "For God so loved the world, that he gave his only begotten Son, that whosoever believeth in him should not perish, but have everlasting life."
    },
    @{
        id    = "quote-trap"
        type  = "quote"
        rate  = 1
        refs  = @()
        text  = "The food pantry served one hundred nineteen families last month, and the Christmas program is on the twenty eighth. Come hungry, and bring a friend."
    },

    # Correction lane: the speaker takes a reference back and names the one
    # they meant. Gold is the corrected passage the queue should keep.
    @{
        id    = "romans-correction"
        type  = "correction"
        rate  = 0
        refs  = @("Romans 8:29")
        text  = "Let us read Romans chapter eight verse twenty eight. No wait, I mean Romans chapter eight verse twenty nine."
    },
    @{
        id    = "john-correction"
        type  = "correction"
        rate  = -1
        refs  = @("John 3:17")
        text  = "Turn to John chapter three verse sixteen. Scratch that, let us go to John chapter three verse seventeen instead."
    },
    @{
        id    = "correction-no-prior"
        type  = "correction"
        rate  = 1
        refs  = @("Romans 8:28")
        text  = "Actually, I mean, let us turn to Romans chapter eight verse twenty eight. That is the promise we are resting on tonight."
    }
)

$jsonClips = @()
$index = 0
foreach ($seg in $segments) {
    $wavPath = Join-Path $setDir "$($seg.id).wav"
    $voice = $voiceNames[$index % $voiceNames.Count]
    $synth.SelectVoice($voice)
    $synth.Rate = [int]$seg.rate
    $synth.SetOutputToWaveFile((Join-Path (Get-Location) $wavPath))
    $synth.Speak([string]$seg.text)
    $synth.SetOutputToDefaultAudioDevice()

    $entry = [ordered]@{
        id         = $seg.id
        file       = "$($seg.id).wav"
        references = $seg.refs
        transcript = $seg.text
    }
    if ($seg.type) { $entry.type = $seg.type }
    if ($seg.established) { $entry.established = $seg.established }
    $jsonClips += $entry
    "SYNTH $wavPath (rate $($seg.rate), voice $voice)"
    $index++
}
$synth.Dispose()

# Gold references give canonical book IDs + verse numbers for scoring; the
# transcript doubles as WER gold text (SAPI reading is nearly literal).
$manifest = [ordered]@{
    name  = "tts-synthetic"
    clips = $jsonClips
}
$json = $manifest | ConvertTo-Json -Depth 5
# UTF-8 WITHOUT BOM — Go's JSON decoder rejects BOM-prefixed files.
[System.IO.File]::WriteAllText((Join-Path (Get-Location) "$setDir\manifest.json"), $json, [System.Text.UTF8Encoding]::new($false))
"MANIFEST $setDir\manifest.json"
$refs = ($jsonClips | ForEach-Object { $_.references.Count } | Measure-Object -Sum).Sum
"TOTAL references in set: $refs"
