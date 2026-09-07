// Package xerr 提供带错误码的错误。
//
// 错误码是格式的一部分：它出现在错误消息里，也是判定错误类型的依据，
// 所以编号与名字都不能改。
package xerr

import "strconv"

// Code 是错误码。
//
// 编号与名字都是对外可见的：错误消息里会带上它们，判定错误类型也靠它。
// 所以编号一旦定下就不能改，中间的空号是留着的，不要拿来填新码。
type Code int

const (
	// Unspecified 是没有具体分类的错误。
	Unspecified Code = 0

	// 1xx 是操作层面的错误：找不到、重名、状态不对、超限。
	FileNotFound                Code = 101
	DatabaseShutdown            Code = 102
	InvalidDatabase             Code = 103
	FileSizeExceeded            Code = 105
	CollectionLimitExceeded     Code = 106
	IndexDropID                 Code = 108
	IndexDuplicateKey           Code = 110
	InvalidIndexKey             Code = 111
	IndexNotFound               Code = 112
	InvalidDBRef                Code = 113
	LockTimeout                 Code = 120
	InvalidCommand              Code = 121
	AlreadyExistsCollectionName Code = 122
	AlreadyOpenDatafile         Code = 124
	InvalidTransactionState     Code = 126
	IndexNameLimitExceeded      Code = 128
	InvalidIndexName            Code = 129
	InvalidCollectionName       Code = 130
	TempEngineAlreadyDefined    Code = 131
	InvalidExpressionType       Code = 132
	CollectionNotFound          Code = 133
	CollectionAlreadyExist      Code = 134
	IndexAlreadyExist           Code = 135
	InvalidUpdateField          Code = 136
	EngineDisposed              Code = 137
)

const (
	// 2xx 是数据与映射层面的错误：格式不对、类型转不过去、口令不对。
	InvalidFormat               Code = 200
	DocumentMaxDepth            Code = 201
	InvalidCtor                 Code = 202
	UnexpectedToken             Code = 203
	InvalidDataType             Code = 204
	PropertyNotMapped           Code = 206
	InvalidTypedName            Code = 207
	PropertyReadWrite           Code = 209
	InitialSizeCryptoNotSupport Code = 210
	InvalidInitialSize          Code = 211
	InvalidNullCharString       Code = 212
	InvalidFreeSpacePage        Code = 213
	DataTypeNotAssignable       Code = 214
	AvoidUseOfProcess           Code = 215
	NotEncrypted                Code = 216
	InvalidPassword             Code = 217
	IllegalDeserializationType  Code = 218
	EntityInitializationFailed  Code = 219
	MapperNotFound              Code = 220
	MappingError                Code = 221
)

const (
	// InvalidDatafileState 表示文件本身的状态不可用。
	//
	// 它落在 9xx，因而是 [Code.Critical] 认定的严重错误：重试没有意义，
	// 要先把文件修好。
	InvalidDatafileState Code = 999
)

// codeNames 是错误码的名字，会出现在错误消息里。
var codeNames = map[Code]string{
	Unspecified:                 "UNSPECIFIED",
	FileNotFound:                "FILE_NOT_FOUND",
	DatabaseShutdown:            "DATABASE_SHUTDOWN",
	InvalidDatabase:             "INVALID_DATABASE",
	FileSizeExceeded:            "FILE_SIZE_EXCEEDED",
	CollectionLimitExceeded:     "COLLECTION_LIMIT_EXCEEDED",
	IndexDropID:                 "INDEX_DROP_ID",
	IndexDuplicateKey:           "INDEX_DUPLICATE_KEY",
	InvalidIndexKey:             "INVALID_INDEX_KEY",
	IndexNotFound:               "INDEX_NOT_FOUND",
	InvalidDBRef:                "INVALID_DBREF",
	LockTimeout:                 "LOCK_TIMEOUT",
	InvalidCommand:              "INVALID_COMMAND",
	AlreadyExistsCollectionName: "ALREADY_EXISTS_COLLECTION_NAME",
	AlreadyOpenDatafile:         "ALREADY_OPEN_DATAFILE",
	InvalidTransactionState:     "INVALID_TRANSACTION_STATE",
	IndexNameLimitExceeded:      "INDEX_NAME_LIMIT_EXCEEDED",
	InvalidIndexName:            "INVALID_INDEX_NAME",
	InvalidCollectionName:       "INVALID_COLLECTION_NAME",
	TempEngineAlreadyDefined:    "TEMP_ENGINE_ALREADY_DEFINED",
	InvalidExpressionType:       "INVALID_EXPRESSION_TYPE",
	CollectionNotFound:          "COLLECTION_NOT_FOUND",
	CollectionAlreadyExist:      "COLLECTION_ALREADY_EXIST",
	IndexAlreadyExist:           "INDEX_ALREADY_EXIST",
	InvalidUpdateField:          "INVALID_UPDATE_FIELD",
	EngineDisposed:              "ENGINE_DISPOSED",
	InvalidFormat:               "INVALID_FORMAT",
	DocumentMaxDepth:            "DOCUMENT_MAX_DEPTH",
	InvalidCtor:                 "INVALID_CTOR",
	UnexpectedToken:             "UNEXPECTED_TOKEN",
	InvalidDataType:             "INVALID_DATA_TYPE",
	PropertyNotMapped:           "PROPERTY_NOT_MAPPED",
	InvalidTypedName:            "INVALID_TYPED_NAME",
	PropertyReadWrite:           "PROPERTY_READ_WRITE",
	InitialSizeCryptoNotSupport: "INITIALSIZE_CRYPTO_NOT_SUPPORTED",
	InvalidInitialSize:          "INVALID_INITIALSIZE",
	InvalidNullCharString:       "INVALID_NULL_CHAR_STRING",
	InvalidFreeSpacePage:        "INVALID_FREE_SPACE_PAGE",
	DataTypeNotAssignable:       "DATA_TYPE_NOT_ASSIGNABLE",
	AvoidUseOfProcess:           "AVOID_USE_OF_PROCESS",
	NotEncrypted:                "NOT_ENCRYPTED",
	InvalidPassword:             "INVALID_PASSWORD",
	IllegalDeserializationType:  "ILLEGAL_DESERIALIZATION_TYPE",
	EntityInitializationFailed:  "ENTITY_INITIALIZATION_FAILED",
	MapperNotFound:              "MAPPER_NOT_FOUND",
	MappingError:                "MAPPING_ERROR",
	InvalidDatafileState:        "INVALID_DATAFILE_STATE",
}

// String 返回错误码的名字，没登记过的就返回数字。
func (c Code) String() string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return strconv.Itoa(int(c))
}

// Error 让错误码自己也是一个 error，这样 errors.Is 可以直接拿它当目标比。
//
// 只出编号与名字，具体说明在 [Error] 里。
func (c Code) Error() string { return "[" + strconv.Itoa(int(c)) + " " + c.String() + "]" }

// Critical 报告这个错误是不是「文件坏了」这一类。
//
// 界线划在 900：以下的是这一次操作没做成，以上的是这份文件不能再照常用了。
func (c Code) Critical() bool { return c >= 900 }
