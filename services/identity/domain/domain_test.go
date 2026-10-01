package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRegistrationValidate(t *testing.T) {
	today := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ok := Registration{Phone: "+2348012345678", FullName: "Ada Obi-Okafor", DateOfBirth: time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC), BVN: "22345678901", PIN: "482915"}
	if err := ok.Validate(today); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}
	mutations := map[string]func(*Registration){
		"local phone format": func(r *Registration) { r.Phone = "08012345678" },
		"empty name":         func(r *Registration) { r.FullName = " " },
		"name with digits":   func(r *Registration) { r.FullName = "Ada 123" },
		"short bvn":          func(r *Registration) { r.BVN = "123" },
		"repeated pin":       func(r *Registration) { r.PIN = "777777" },
		"sequential pin":     func(r *Registration) { r.PIN = "123456" },
		"future dob":         func(r *Registration) { r.DateOfBirth = today.AddDate(0, 0, 1) },
		"turns 18 tomorrow":  func(r *Registration) { r.DateOfBirth = time.Date(2008, 10, 2, 0, 0, 0, 0, time.UTC) },
	}
	for name, mutate := range mutations {
		r := ok
		mutate(&r)
		if err := r.Validate(today); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// Exactly 18 today is accepted.
	r := ok
	r.DateOfBirth = time.Date(2008, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := r.Validate(today); err != nil {
		t.Errorf("18th birthday today should be accepted: %v", err)
	}
}

func TestAgeOn(t *testing.T) {
	dob := time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC)
	if got := AgeOn(dob, time.Date(2018, 2, 28, 0, 0, 0, 0, time.UTC)); got != 17 {
		t.Errorf("leap-day birthday, day before: got %d want 17", got)
	}
	if got := AgeOn(dob, time.Date(2018, 3, 1, 0, 0, 0, 0, time.UTC)); got != 18 {
		t.Errorf("leap-day birthday, 1 March: got %d want 18", got)
	}
}
