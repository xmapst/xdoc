package xdoc

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
)

// pragma 是一个头页配置项的读、校验、写三件套。
//
// 拆成三步是因为写要落进事务的提交回调里，而校验必须在开事务之前做完——
// 在提交回调里报错已经太晚了。
type pragma struct {
	// get 从头页读出当前值。
	get func(h *xpage.HeaderPage) *Value

	// validate 判断新值能不能接受，h 是当前头页（拿来做与现状相关的检查）。
	// 只读项在这里直接报错。
	validate func(v *Value, h *xpage.HeaderPage) error

	// set 把值写进头页。
	//
	// h 为 nil 时它**只做类型转换、不写**，用来在开事务前先把转不出来的值挡掉。
	set func(v *Value, h *xpage.HeaderPage) error
}

// pragmas 是全部可用配置项，键是大写名。
var pragmas = map[string]pragma{
	"USER_VERSION": {
		get:      func(h *xpage.HeaderPage) *Value { return xbson.Int32(h.UserVersion()) },
		validate: func(*Value, *xpage.HeaderPage) error { return nil },
		set: func(v *Value, h *xpage.HeaderPage) error {
			n, err := pragmaInt32(v)
			if err != nil || h == nil {
				return err
			}
			h.SetUserVersion(n)
			return nil
		},
	},
	"COLLATION": {
		get: func(h *xpage.HeaderPage) *Value { return xbson.String(h.Collation().String()) },

		validate: func(*Value, *xpage.HeaderPage) error {
			return fmt.Errorf("xdoc: pragma COLLATION is read only, use Rebuild to change it")
		},
		set: func(*Value, *xpage.HeaderPage) error { return nil },
	},
	"TIMEOUT": {
		get: func(h *xpage.HeaderPage) *Value { return xbson.Int32(int32(h.Timeout() / time.Second)) },
		validate: func(v *Value, _ *xpage.HeaderPage) error {
			if v.Compare(xbson.Int32(0), xcoll.Binary) <= 0 {
				return fmt.Errorf("xdoc: pragma TIMEOUT must be greater than zero")
			}
			return nil
		},
		set: func(v *Value, h *xpage.HeaderPage) error {
			n, err := pragmaInt32(v)
			if err != nil || h == nil {
				return err
			}
			h.SetTimeout(time.Duration(n) * time.Second)
			return nil
		},
	},
	"LIMIT_SIZE": {
		get: func(h *xpage.HeaderPage) *Value { return xbson.Int64(h.LimitSize()) },
		validate: func(v *Value, h *xpage.HeaderPage) error {
			if v.Compare(xbson.Int32(4*xpage.PageSize), xcoll.Binary) < 0 {
				return fmt.Errorf("xdoc: pragma LIMIT_SIZE must be at least 4 pages (%d bytes)", 4*xpage.PageSize)
			}

			n, err := pragmaInt64(v)
			if err != nil {
				return err
			}

			if h != nil && n < (int64(h.LastPageID())+1)*xpage.PageSize {
				return fmt.Errorf("xdoc: pragma LIMIT_SIZE must be greater or equal to the current file size")
			}
			return nil
		},
		set: func(v *Value, h *xpage.HeaderPage) error {
			n, err := pragmaInt64(v)
			if err != nil || h == nil {
				return err
			}
			h.SetLimitSize(n)
			return nil
		},
	},
	"UTC_DATE": {
		get:      func(h *xpage.HeaderPage) *Value { return xbson.Boolean(h.UTCDate()) },
		validate: func(*Value, *xpage.HeaderPage) error { return nil },
		set: func(v *Value, h *xpage.HeaderPage) error {
			b, err := pragmaBool(v)
			if err != nil || h == nil {
				return err
			}
			h.SetUTCDate(b)
			return nil
		},
	},
	"CHECKPOINT": {
		get: func(h *xpage.HeaderPage) *Value { return xbson.Int32(h.Checkpoint()) },
		validate: func(v *Value, _ *xpage.HeaderPage) error {
			if v.Compare(xbson.Int32(0), xcoll.Binary) < 0 {
				return fmt.Errorf("xdoc: pragma CHECKPOINT must be greater or equal to zero")
			}
			return nil
		},
		set: func(v *Value, h *xpage.HeaderPage) error {
			n, err := pragmaInt32(v)
			if err != nil || h == nil {
				return err
			}
			h.SetCheckpoint(n)
			return nil
		},
	},
}

// PragmaNames 按字典序返回全部配置项名。
func PragmaNames() []string {
	names := make([]string, 0, len(pragmas))
	for n := range pragmas {
		names = append(names, n)
	}

	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// Pragma 读一个配置项的当前值，名字不区分大小写。
//
// 名字不认识时错误里会列出全部可用名，省得去翻文档。
func (db *DB) Pragma(name string) (*Value, error) {
	p, ok := pragmas[strings.ToUpper(name)]
	if !ok {
		return nil, fmt.Errorf("xdoc: pragma %q does not exist, known ones are %s",
			name, strings.Join(PragmaNames(), ", "))
	}

	rel, err := db.enter(context.Background())
	if err != nil {
		return nil, err
	}
	defer rel()
	v, err := db.core.HeaderValue(func(h *xpage.HeaderPage) (*Value, error) { return p.get(h), nil })
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SetPragma 改一个配置项，返回值有没有真的变过。
//
// 值与现状相同就直接返回 false，不开事务也不算作在事务中——所以把一个已经是
// 当前值的设置写在事务里不会失败。真要改则必须在事务外：改的是头页，
// 而头页的改动是随提交一起落的，嵌在别人的事务里语义不清。
//
// 顺序是先比、再校验、再试转换、最后才开事务：一旦事务开了，中途报错就要回滚，
// 而这些检查全都不需要事务也能做完。写头页挂在提交回调上，事务没提交成功
// 头页也就没变。
//
// 改完重读一次运行期参数，让新的超时、检查点这些立刻生效。
func (db *DB) SetPragma(ctx context.Context, name string, v *Value) (bool, error) {
	key := strings.ToUpper(name)
	p, ok := pragmas[key]
	if !ok {
		return false, fmt.Errorf("xdoc: pragma %q does not exist, known ones are %s",
			name, strings.Join(PragmaNames(), ", "))
	}
	if v == nil {
		v = xbson.Null
	}
	rel, err := db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()

	var same bool
	if err := db.core.WithHeader(func(h *xpage.HeaderPage) error {
		same = p.get(h).Compare(v, xcoll.Binary) == 0
		return nil
	}); err != nil {
		return false, err
	}
	if same {
		return false, nil
	}
	if db.currentTx() != nil {
		return false, db.rejectInTransaction()
	}
	if err := db.core.WithHeader(func(h *xpage.HeaderPage) error { return p.validate(v, h) }); err != nil {
		return false, err
	}

	if err := p.set(v, nil); err != nil {
		return false, err
	}

	tx, err := db.core.Begin(ctx)
	if err != nil {
		return false, err
	}
	tx.OnCommit(func(h *xpage.HeaderPage) error { return p.set(v, h) })
	if err := tx.Commit(); err != nil {
		return false, err
	}

	db.core.ReloadPragmas()
	return true, nil
}

// pragmaInt32 把值转成 int32，超出范围报错而不是截断。
func pragmaInt32(v *Value) (int32, error) {
	n, err := pragmaInt64(v)
	if err != nil {
		return 0, err
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("xdoc: pragma value %d does not fit in an int32", n)
	}
	return int32(n), nil
}

// pragmaInt64 把值转成 int64。
//
// 数字、布尔、十进制数字串都收；浮点按**银行家舍入**取整（.5 进到偶数），
// 与十进制那条路一致。
func pragmaInt64(v *Value) (int64, error) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return int64(n), nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return n, nil
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		r := math.RoundToEven(f)
		if math.IsNaN(r) || r < math.MinInt64 || r >= math.MaxInt64+1.0 {
			return 0, fmt.Errorf("xdoc: pragma value %v is out of range for an integer", f)
		}
		return int64(r), nil
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return decimalToInt64(d)
	case xbson.TypeBoolean:
		b, _ := v.AsBoolean()
		if b {
			return 1, nil
		}
		return 0, nil
	case xbson.TypeString:
		s, _ := v.AsString()
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("xdoc: pragma value %q is not an integer", s)
		}
		return n, nil
	}
	return 0, fmt.Errorf("xdoc: pragma value of type %s is not an integer", v.Type())
}

// decimalToInt64 把十进制数按银行家舍入转成 int64。
//
// 走 big.Rat 而不是先转 float64：十进制的有效位比 float64 多，
// 先落到浮点会在舍入之前就丢掉决定进位方向的那一位。
func decimalToInt64(d xbin.Decimal) (int64, error) {
	s := d.String()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("xdoc: pragma value %q is not an integer", s)
	}

	num, den := r.Num(), r.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() != 0 {
		twice := new(big.Int).Abs(rem)
		twice.Lsh(twice, 1)
		switch twice.Cmp(den) {
		case 1:
			q.Add(q, big.NewInt(int64(rem.Sign())))
		case 0:
			if q.Bit(0) == 1 {
				q.Add(q, big.NewInt(int64(rem.Sign())))
			}
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("xdoc: pragma value %q is out of range for an integer", s)
	}
	return q.Int64(), nil
}

// pragmaBool 取布尔值。
//
// 这里不接受 0/1 或 "true" 这类替代写法——布尔配置项只有两个取值，
// 容忍别的写法只会让"设了但没生效"更难查。
func pragmaBool(v *Value) (bool, error) {
	if b, ok := v.AsBoolean(); ok {
		return b, nil
	}
	return false, fmt.Errorf("xdoc: pragma value of type %s is not a boolean", v.Type())
}
