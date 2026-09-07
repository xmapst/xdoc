package xbexpr

import "time"

// timeNow 是 NOW、TODAY 之类取当前时间的方法唯一的时钟入口。
//
// 单独抽出来，是为了让时间相关的行为有一个可替换的点。
var timeNow = time.Now
