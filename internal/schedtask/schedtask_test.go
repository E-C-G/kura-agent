package schedtask

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/E-C-G/kura-agent/pkg/config"
)

var day = time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local)

func params() Params {
	return Params{
		UserID: "S-1-5-21-1-2-3-1001", Command: `C:\Program Files\Kura\kura-agent.exe`,
		Arguments: "run --quiet", WorkingDir: `C:\ProgramData\Kura`,
		RunNowSIDs: []string{"S-1-5-21-1-2-3-1002"}, Now: day,
	}
}

func dur(d time.Duration) config.Duration { return config.Duration{Duration: d} }

func mustXML(t *testing.T, s config.Schedule) string {
	t.Helper()
	x, err := XML(s, params())
	if err != nil {
		t.Fatal(err)
	}
	// Le résultat doit rester un XML bien formé.
	if err := xml.Unmarshal([]byte(strings.Replace(x, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)), new(task)); err != nil {
		t.Fatalf("XML mal formé : %v\n%s", err, x)
	}
	return x
}

func contains(t *testing.T, x string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(x, w) {
			t.Errorf("%q absent du XML :\n%s", w, x)
		}
	}
}

func TestWeekdaysAtNight(t *testing.T) {
	x := mustXML(t, config.Schedule{Type: "daily", Time: "22:30", Days: []string{"mon", "tue", "wed", "thu", "fri"},
		RunIfMissed: true, MaxDuration: dur(6 * time.Hour)})
	contains(t, x,
		`<URI>\Kura\Sync</URI>`,
		"<StartBoundary>2026-10-26T22:30:00</StartBoundary>",
		"<ScheduleByWeek>", "<Monday></Monday>", "<Friday></Friday>", "<WeeksInterval>1</WeeksInterval>",
		"<UserId>S-1-5-21-1-2-3-1001</UserId>", "<LogonType>S4U</LogonType>", "<RunLevel>HighestAvailable</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<StartWhenAvailable>true</StartWhenAvailable>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<ExecutionTimeLimit>PT6H15M</ExecutionTimeLimit>",
		`<Command>C:\Program Files\Kura\kura-agent.exe</Command>`, "<Arguments>run --quiet</Arguments>",
		"<SecurityDescriptor>D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;S-1-5-21-1-2-3-1002)</SecurityDescriptor>")
	for _, absent := range []string{"Saturday", "Sunday", "ScheduleByDay", "Repetition"} {
		if strings.Contains(x, absent) {
			t.Errorf("%s inattendu", absent)
		}
	}
}

func TestEveryDayAndHourly(t *testing.T) {
	x := mustXML(t, config.Schedule{Type: "daily", Time: "01:05", MaxDuration: dur(90 * time.Minute)})
	contains(t, x, "<ScheduleByDay>", "<DaysInterval>1</DaysInterval>", "<StartWhenAvailable>false</StartWhenAvailable>",
		"<ExecutionTimeLimit>PT1H45M</ExecutionTimeLimit>", "T01:05:00")
	h := mustXML(t, config.Schedule{Type: "hourly", MaxDuration: dur(45 * time.Minute)})
	contains(t, h, "<Interval>PT1H</Interval>", "<Duration>P1D</Duration>", "T00:00:00", "<ScheduleByDay>")
}

func TestWeekly(t *testing.T) {
	x := mustXML(t, config.Schedule{Type: "weekly", Time: "20:00", Days: []string{"sat"}})
	contains(t, x, "<Saturday></Saturday>", "<ExecutionTimeLimit>PT6H15M</ExecutionTimeLimit>")
	if _, err := XML(config.Schedule{Type: "weekly", Time: "20:00"}, params()); err == nil {
		t.Error("weekly sans jour accepté")
	}
}

func TestRejects(t *testing.T) {
	p := params()
	p.RunNowSIDs = []string{"S-1-5-21-1)(A;;FA;;;WD"}
	if _, err := XML(config.Schedule{Type: "daily", Time: "22:30"}, p); err == nil {
		t.Error("SID qui injecte une ACE accepté")
	}
	if _, err := XML(config.Schedule{Type: "daily", Time: "25:00"}, params()); err == nil {
		t.Error("heure invalide acceptée")
	}
	if _, err := XML(config.Schedule{Type: "daily", Time: "22:30"}, Params{Command: "x"}); err == nil {
		t.Error("compte vide accepté")
	}
}

func TestEscaping(t *testing.T) {
	p := params()
	p.Command = `C:\Kura & Co\<agent>.exe`
	x, err := XML(config.Schedule{Type: "daily", Time: "22:30"}, p)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, x, `C:\Kura &amp; Co\&lt;agent&gt;.exe`)
}

func TestUTF16(t *testing.T) {
	b := UTF16("é\n")
	if b[0] != 0xFF || b[1] != 0xFE {
		t.Fatal("BOM UTF-16 LE absent")
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
	}
	if got := string(utf16.Decode(u)); got != "é\r\n" {
		t.Fatalf("décodage %q", got)
	}
}

func TestISODuration(t *testing.T) {
	for d, want := range map[time.Duration]string{6*time.Hour + 15*time.Minute: "PT6H15M", 90 * time.Second: "PT1M30S", 0: "PT0S", 2 * time.Hour: "PT2H"} {
		if got := isoDuration(d); got != want {
			t.Errorf("isoDuration(%v) = %s, attendu %s", d, got, want)
		}
	}
}
