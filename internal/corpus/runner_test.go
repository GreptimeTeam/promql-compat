package corpus

import (
	"context"
	"testing"
)

func TestLoadSeriesHonoursClear(t *testing.T) {
	series, err := LoadSeries(context.Background(), `
load 1m
  before_clear 1 2 3

clear

load 1m
  after_clear 4 5 6
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Labels.Get("__name__") != "after_clear" {
		t.Fatalf("got %v, want only after_clear", series)
	}
}
