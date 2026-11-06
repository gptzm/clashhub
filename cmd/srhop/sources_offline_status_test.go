package main

import (
	"testing"
)

func TestSourcesReportKeepsOfflineStatusAfterLateOnlineError(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: sourceIDOffice91, Label: "Office", Type: SourceTypeSubscription, Enabled: true}}, map[string]string{"node": sourceIDOffice91}, "")
	if err := writeOffice91Mode(office91ModeOffline); err != nil {
		t.Fatal(err)
	}
	recordOffice91OfflineStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "ok"})
	recordSubscriptionStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "error", Error: "network"})
	report := (&statsIntegration{}).buildSourcesReport(60)
	if len(report.Sources) != 1 || report.Sources[0].Status != "ok" || report.Sources[0].LastError != "" {
		t.Fatalf("late online diagnostic leaked: %+v", report.Sources)
	}
	recordOffice91OfflineStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "error", Error: "offline-reload-failed"})
	recordSubscriptionStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "error", Error: "offline-mode"})
	report = (&statsIntegration{}).buildSourcesReport(60)
	if report.Sources[0].LastError != "offline-reload-failed" {
		t.Fatalf("real offline failure hidden: %+v", report.Sources[0])
	}
	if err := writeOffice91Mode(office91ModeOnline); err != nil {
		t.Fatal(err)
	}
	recordSubscriptionStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "error", Error: "network"})
	report = (&statsIntegration{}).buildSourcesReport(60)
	if report.Sources[0].LastError != "network" {
		t.Fatal("online diagnostic not restored")
	}
}
