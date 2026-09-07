package xbin

import (
	"fmt"
	"time"
)

const (
	// TicksPerMillisecond 是一毫秒有多少个计时单位（一个单位 100 纳秒）。
	TicksPerMillisecond = 10_000

	// UnixEpochTicks 是 Unix 纪元距 0001-01-01 的计时单位数。
	UnixEpochTicks int64 = 621_355_968_000_000_000

	// MinTicks、MaxTicks 是可表示的计时单位范围，对应 0001-01-01 到 9999-12-31。
	MinTicks int64 = 0
	MaxTicks int64 = 3_155_378_975_999_999_999

	// MinUnixMillis、MaxUnixMillis 是可表示范围对应的 Unix 毫秒。
	//
	// 上界是 10000-01-01 那一刻，比 [MaxTicks] 对应的时刻晚一点点：
	// 毫秒精度装不下 9999-12-31 23:59:59.9999999，所以这两个端点是**特判**映射的，
	// 见 [UnixMillisToTime] 与 [TimeToUnixMillis]。
	MinUnixMillis int64 = -62_135_596_800_000
	MaxUnixMillis int64 = 253_402_300_800_000
)

const (
	// ticksPerSecond 是一秒有多少个计时单位。
	ticksPerSecond = 10_000_000
	// minUnixSec 是 0001-01-01 的 Unix 秒。
	minUnixSec = -62_135_596_800
)

var (
	// timeMin、timeMax 是可表示的时间端点，都在 UTC。
	timeMin = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	timeMax = time.Date(9999, 12, 31, 23, 59, 59, 999_999_900, time.UTC)
)

// TimeMin 返回可表示的最早时刻。
func TimeMin() time.Time { return timeMin }

// TimeMax 返回可表示的最晚时刻。
func TimeMax() time.Time { return timeMax }

// TicksToTime 把计时单位转成时间。
//
// 两个端点直接给常量：那两个时刻的纳秒部分不是 100 的整数倍能精确表达的，
// 走通用算式会有偏差。
func TicksToTime(ticks int64) (time.Time, error) {
	if ticks < MinTicks || ticks > MaxTicks {
		return time.Time{}, fmt.Errorf("xbin: ticks %d out of range %d..%d", ticks, MinTicks, MaxTicks)
	}
	switch ticks {
	case MinTicks:
		return timeMin, nil
	case MaxTicks:
		return timeMax, nil
	}

	return time.Unix(minUnixSec+ticks/ticksPerSecond, (ticks%ticksPerSecond)*100).UTC(), nil
}

// TimeToTicks 把时间转成计时单位。
//
// 纳秒整除 100，不足一个单位的部分被截掉。
func TimeToTicks(t time.Time) (int64, error) {
	u := t.UTC()
	if u.Before(timeMin) || u.After(timeMax) {
		return 0, fmt.Errorf("xbin: time %s out of representable range", u.Format(time.RFC3339Nano))
	}

	return (u.Unix()-minUnixSec)*ticksPerSecond + int64(u.Nanosecond())/100, nil
}

// UnixMillisToTime 把 Unix 毫秒转成时间。
//
// 两个端点特判：毫秒精度表达不了那两个时刻，用最接近的边界值代表它们。
func UnixMillisToTime(ms int64) (time.Time, error) {
	switch ms {
	case MinUnixMillis:
		return timeMin, nil
	case MaxUnixMillis:
		return timeMax, nil
	}
	if ms < MinUnixMillis || ms > MaxUnixMillis {
		return time.Time{}, fmt.Errorf("xbin: unix millis %d out of range %d..%d", ms, MinUnixMillis, MaxUnixMillis)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// TimeToUnixMillis 把时间转成 Unix 毫秒。
//
// 两个端点特判，与 [UnixMillisToTime] 对称，来回转能回到原处。
func TimeToUnixMillis(t time.Time) (int64, error) {
	u := t.UTC()
	switch {
	case u.Equal(timeMin):
		return MinUnixMillis, nil
	case u.Equal(timeMax):
		return MaxUnixMillis, nil
	case u.Before(timeMin) || u.After(timeMax):
		return 0, fmt.Errorf("xbin: time %s out of representable range", u.Format(time.RFC3339Nano))
	}
	return u.UnixMilli(), nil
}

// TruncateToMillis 把时间截到毫秒。
//
// 两个端点不动：截它们就出了可表示范围。
func TruncateToMillis(t time.Time) time.Time {
	u := t.UTC()
	if u.Equal(timeMin) || u.Equal(timeMax) {
		return u
	}
	return u.Truncate(time.Millisecond)
}
