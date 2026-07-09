package money

import "testing"

func TestParse_ValidMinorUnits(t *testing.T) {
	a, err := Parse("10000", "USDC")
	if err != nil {
		t.Fatal(err)
	}
	if a.Minor != 10000 || a.Currency != "USDC" {
		t.Fatalf("got %+v", a)
	}
	if a.String() != "10000" {
		t.Fatalf("String() = %q", a.String())
	}
}

func TestParse_RejectsFloatSignAndJunk(t *testing.T) {
	for _, s := range []string{"0.01", "-5", "1e3", "abc", "", " 10"} {
		if _, err := Parse(s, "USDC"); err == nil {
			t.Errorf("Parse(%q) should fail", s)
		}
	}
}

func TestParse_RejectsEmptyCurrency(t *testing.T) {
	if _, err := Parse("100", ""); err == nil {
		t.Fatal("empty currency should fail")
	}
}

func TestAddAndGTE_CurrencyMismatch(t *testing.T) {
	usd, _ := Parse("100", "USDC")
	eur := New(100, "EURC")
	if _, err := usd.Add(eur); err == nil {
		t.Fatal("cross-currency Add should fail")
	}
	if _, err := usd.GTE(eur); err == nil {
		t.Fatal("cross-currency GTE should fail")
	}
	ok, err := usd.GTE(New(50, "USDC"))
	if err != nil || !ok {
		t.Fatalf("100 >= 50 should be true, got %v %v", ok, err)
	}
}
