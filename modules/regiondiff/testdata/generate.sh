#!/bin/sh
# Generates the audio fixtures for the compare-by-audio tests (audio_test.go):
# a synthetic 40-second "show" (pink noise with an irregular, speech-like
# loudness) and three "ads" (other noise, other rhythms), cut together and
# encoded as a re-encoding host would, each file with its own settings.
# No real episode audio. Needs ffmpeg with libmp3lame; the output is checked
# in, so the tests don't need ffmpeg.
set -eu
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Loudness that varies like speech: several slow, unrelated rhythms.
show_volume='0.05+0.95*pow(abs(sin(2*PI*0.71*t)*sin(2*PI*0.23*t+1.3)*sin(2*PI*1.87*t+0.4)),2)'
ffmpeg -v error -f lavfi -i "anoisesrc=color=pink:seed=11:duration=40:sample_rate=48000" \
	-af "volume='$show_volume':eval=frame" -ac 1 "$work/show.wav"
ad() { # name, seconds, colour, seed, rhythm
	ffmpeg -v error -f lavfi -i "anoisesrc=color=$3:seed=$4:duration=$2:sample_rate=48000" \
		-af "volume='0.1+0.9*abs(sin(2*PI*$5*t))':eval=frame" -ac 1 "$work/$1.wav"
}
ad ad1 8 brown 21 0.9
ad ad2 5 white 22 2.3
ad ad3 6 brown 23 1.4
ad unrelated 20 pink 24 0.37

piece() { # name, from, to
	ffmpeg -v error -i "$work/show.wav" -af "atrim=$2:$3,asetpts=N/SR/TB" "$work/$1.wav"
}
piece s0-15 0 15
piece s15-40 15 40
piece s0-25 0 25
piece s25-40 25 40
concat() { # output, pieces...
	out=$1
	shift
	list="$work/$out.txt"
	: >"$list"
	for p in "$@"; do echo "file '$work/$p.wav'" >>"$list"; done
	ffmpeg -v error -f concat -safe 0 -i "$list" "$work/$out.wav"
}
concat other-ads s0-15 ad1 s15-40 ad2
concat home-ad s0-25 ad3 s25-40

encode() { # input, output, sample rate, bitrate
	ffmpeg -v error -y -i "$work/$1.wav" -ar "$3" -b:a "$4" -c:a libmp3lame -write_xing 0 -id3v2_version 0 "$2"
}
# The home download without ads, as the host serves the original.
encode show home-clean.mp3 32000 32k
# The other region's copy: ads at 15 s (8 s) and at the end (5 s),
# re-encoded at another sample rate (MPEG-2).
encode other-ads other-ads.mp3 22050 32k
# A home download with an ad of its own at 25 s (6 s).
encode home-ad home-ad.mp3 32000 32k
# The same show re-encoded, without ads: nothing differs.
encode show other-plain.mp3 24000 32k
# The home copy with its ad again, as a stereo, variable-bitrate encoder
# would make it: joint stereo (mid/side), frame sizes that vary.
ffmpeg -v error -y -i "$work/home-ad.wav" -af "pan=stereo|c0=c0|c1=0.8*c0" -ar 44100 \
	-c:a libmp3lame -q:a 9 -joint_stereo 1 -write_xing 0 -id3v2_version 0 home-ad-vbr-stereo.mp3
# Something else altogether.
encode unrelated unrelated.mp3 32000 32k
