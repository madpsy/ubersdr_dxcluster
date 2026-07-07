package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// TestTappableCell confirms the picker cell implements Tappable/DoubleTappable
// and reports the correct row for each gesture. The glfw driver routes a click
// to the innermost object implementing these interfaces, so a table of these
// cells receives single- and double-clicks directly.
func TestTappableCell(t *testing.T) {
	test.NewApp()

	var tappedRow, tappedCol, doubledRow = -1, -1, -1
	c := newTappableCell()
	c.row, c.col = 3, 1
	c.onTap = func(row, col int) { tappedRow, tappedCol = row, col }
	c.onDoubleTap = func(row int) { doubledRow = row }

	test.Tap(c)
	if tappedRow != 3 || tappedCol != 1 {
		t.Fatalf("Tapped: got row=%d col=%d, want row=3 col=1", tappedRow, tappedCol)
	}

	test.DoubleTap(c)
	if doubledRow != 3 {
		t.Fatalf("DoubleTapped: got row=%d, want 3", doubledRow)
	}
}
