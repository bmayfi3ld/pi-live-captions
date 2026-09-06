package cli

import (
	"testing"
	"time"

	"livecaption/internal/audio"
)

func TestNoiseFlags(t *testing.T) {
	t.Setenv("LIVECAPTION_NOISE_THRESHOLD_DBFS", "")
	t.Setenv("LIVECAPTION_NOISE_RELEASE", "")
	file := writeTempFile(t)
	_, cli, err := Parse([]string{"replay", file})
	if err != nil {
		t.Fatal(err)
	}
	if cli.Replay.NoiseThresholdDBFS != audio.DefaultNoiseThresholdDBFS || cli.Replay.NoiseRelease != audio.DefaultNoiseRelease {
		t.Fatal("wrong defaults")
	}
	for _, args := range [][]string{
		{"--noise-threshold-dbfs=1"}, {"--noise-threshold-dbfs=-101"}, {"--noise-threshold-dbfs=NaN"}, {"--noise-threshold-dbfs=Inf"}, {"--noise-release=-1s"}, {"--noise-release=61s"},
	} {
		if _, _, err := Parse(append([]string{"replay", file}, args...)); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	_, cli, err = Parse([]string{"replay", file, "--noise-threshold-dbfs=0", "--noise-release=0s"})
	if err != nil {
		t.Fatal(err)
	}
	if cli.Replay.NoiseThresholdDBFS != 0 || cli.Replay.NoiseRelease != 0 {
		t.Fatal("explicit zeros lost")
	}
	t.Setenv("LIVECAPTION_NOISE_THRESHOLD_DBFS", "-65")
	t.Setenv("LIVECAPTION_NOISE_RELEASE", "12s")
	_, cli, err = Parse([]string{"live", "--device=test"})
	if err != nil {
		t.Fatal(err)
	}
	if cli.Live.NoiseThresholdDBFS != -65 || cli.Live.NoiseRelease != 12*time.Second {
		t.Fatal("environment not honored")
	}
}
