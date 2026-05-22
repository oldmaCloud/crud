package controller

import (
	"context"
	"reflect"
	"strings"
	"sync"

	"github.com/oldmaCloud/crud/orm"
	"github.com/oldmaCloud/crud/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// MaxPerPage 是列表接口单页返回行数的硬上限。它同时作为「未传分页参数」时的兜底，
// 确保任何 GetList 都带 LIMIT，避免 device_tech / login_log 这类大表被整表拉出，
// 也防止超大 perPage 滥用。消费方可在启动时覆盖此值。
var MaxPerPage = 1000

// columnCache 缓存每个模型的合法列名集合（reflect.Type -> map[string]struct{}）。
var columnCache sync.Map

// validColumns 返回模型 T 的合法数据库列名集合，用于对来自 URL 的列名（排序/过滤）
// 做白名单校验，避免把请求里的标识符原样拼进 SQL（注入风险）。结果按类型缓存。
func validColumns[T any]() map[string]struct{} {
	t := reflect.TypeOf(new(T)).Elem()
	if v, ok := columnCache.Load(t); ok {
		return v.(map[string]struct{})
	}
	cols := make(map[string]struct{})
	var namer schema.Namer = schema.NamingStrategy{SingularTable: true}
	if orm.DB != nil {
		namer = orm.DB.NamingStrategy
	}
	if s, err := schema.Parse(new(T), &sync.Map{}, namer); err == nil {
		for _, f := range s.Fields {
			if f.DBName != "" {
				cols[f.DBName] = struct{}{}
			}
		}
	}
	columnCache.Store(t, cols)
	return cols
}

// pageOption 计算受 MaxPerPage 约束的分页选项。perPage<=0（未传）或超限时一律用 MaxPerPage，
// 因此列表查询始终带 LIMIT。
func pageOption(request GetRequestOptions) service.QueryOption {
	perPage := request.PerPage
	if perPage <= 0 || perPage > MaxPerPage {
		perPage = MaxPerPage
	}
	page := request.Page
	if page < 1 {
		page = 1
	}
	return service.WithPage(perPage, perPage*(page-1))
}

// GetRequestOptions is the query options (?opt=val) for GET requests:
//
//	limit=10&offset=4&                 # pagination
//	order_by=id&desc=true&             # ordering
//	filter_by=name&filter_value=John&  # filtering
//	total=true&                        # return total count (all available records under the filter, ignoring pagination)
//	preload=Product&preload=Product.Manufacturer  # preloading: loads nested models as well
//
// It is used in GetListHandler, GetByIDHandler and GetFieldHandler, to bind
// the query parameters in the GET request url.
type GetRequestOptions struct {
	// Limit       int      `form:"limit"`
	Page    int `form:"page"`
	PerPage int `form:"perPage"`
	// Offset      int      `form:"offset"`
	OrderBy     string   `form:"order_by"`
	Descending  bool     `form:"desc"`
	FilterBy    string   `form:"filter_by"`
	FilterValue string   `form:"filter_value"`
	Preload     []string `form:"preload"` // fields to preload
	Total       bool     `form:"total"`   // return total count ?
}

// GetListHandler handles
//
//	GET /T
//
// It returns a list of models.
//
// QueryOptions (See GetRequestOptions for more details):
//
//	limit, offset, order_by, desc, filter_by, filter_value, preload, total.
//
// Response:
//   - 200 OK: { Ts: [{...}, ...] }
//   - 400 Bad Request: { error: "request band failed" }
//   - 422 Unprocessable Entity: { error: "get process failed" }

func FilterByLike(field string, value any) service.QueryOption {
	return func(tx *gorm.DB) *gorm.DB {
		strValue, ok := value.(string)
		if !ok {
			return tx // return unchanged query for unsupported types
		}
		return tx.Where(field+" like ?", "%"+strValue+"%")
	}
}
func GetListHandler[T any]() gin.HandlerFunc {
	return func(c *gin.Context) {
		var request GetRequestOptions
		if err := c.ShouldBind(&request); err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetListHandler: bind request failed")
			ResponseError(c, CodeBadRequest, err)
			return
		}

		options := buildQueryOptions[T](request)
		var options2 []service.QueryOption

		// 动态过滤（field_eq / _like / _lt / _lte / _gt / _gte）。列名来自 URL，
		// 必须先过模型列白名单，否则会把请求里的标识符原样拼进 SQL（注入风险）。
		valid := validColumns[T]()
		for key, value2 := range c.Request.URL.Query() {
			if len(value2) < 1 || len(value2[0]) < 1 {
				continue
			}
			// Extract the first value from the slice
			value := value2[0]
			var fieldName, op string
			switch {
			case strings.HasSuffix(key, "_eq"):
				fieldName, op = strings.TrimSuffix(key, "_eq"), "eq"
			case strings.HasSuffix(key, "_like"):
				fieldName, op = strings.TrimSuffix(key, "_like"), "like"
			case strings.HasSuffix(key, "_lte"):
				fieldName, op = strings.TrimSuffix(key, "_lte"), "<="
			case strings.HasSuffix(key, "_lt"):
				fieldName, op = strings.TrimSuffix(key, "_lt"), "<"
			case strings.HasSuffix(key, "_gte"):
				fieldName, op = strings.TrimSuffix(key, "_gte"), ">="
			case strings.HasSuffix(key, "_gt"):
				fieldName, op = strings.TrimSuffix(key, "_gt"), ">"
			default:
				continue
			}
			if _, ok := valid[fieldName]; !ok {
				logger.WithContext(c).WithField("field", fieldName).
					Warn("GetListHandler: ignore filter on unknown column")
				continue
			}
			switch op {
			case "eq":
				options2 = append(options2, service.FilterBy(fieldName, value))
			case "like":
				options2 = append(options2, FilterByLike(fieldName, value))
			default:
				options2 = append(options2, service.Where(fieldName+" "+op+" ?", value))
			}
		}

		options = append(options, options2...)
		options = append(options, pageOption(request)) // 始终带上限分页，避免整表返回
		var dest []*T
		err := service.GetMany[T](c, &dest, options...)
		if err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetListHandler: GetMany failed")
			ResponseError(c, CodeProcessFailed, err)
			return
		}

		var addition []gin.H
		if request.PerPage > 0 {
			total, err := service.Count[T](c, options2...)
			if err != nil {
				logger.WithContext(c).WithError(err).
					Warn("GetListHandler: getCount failed")
				addition = append(addition, gin.H{"totalError": err.Error()})
			} else {
				addition = append(addition, gin.H{"total": total})
			}
		}
		ResponseSuccess(c, dest, addition...)
	}
}

// GetByIDHandler handles
//
//	GET /T/:idParam
//
// QueryOptions (See GetRequestOptions for more details): preload
//
// Response:
//   - 200 OK: { T: {...} }
//   - 400 Bad Request: { error: "request band failed" }
//   - 422 Unprocessable Entity: { error: "get process failed" }
func GetByIDHandler[T orm.Model](idParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request GetRequestOptions
		if err := c.ShouldBind(&request); err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetByIDHandler: bind request failed")
			ResponseError(c, CodeBadRequest, err)
			return
		}

		options := buildQueryOptions[T](request)

		dest, err := getModelByID[T](c, idParam, options...)
		if err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetByIDHandler: getModelByID failed")
			ResponseError(c, CodeProcessFailed, err)
			return
		}
		ResponseSuccess(c, dest)
	}
}

// GetFieldHandler handles
//
//	GET /T/:idParam/field
//
// QueryOptions (See GetRequestOptions for more details):
//
//	limit, offset, order_by, desc, filter_by, filter_value, preload, total.
//
// Notice, all GetRequestOptions will be conditions for the field, for example:
//
//	GET /user/123/order?preload=Product
//
// Preloads User.Order.Product instead of User.Product.
//
// Response:
//   - 200 OK: { Fs: [{...}, ...] }  // field models
//   - 400 Bad Request: { error: "request band failed" }
//   - 422 Unprocessable Entity: { error: "get process failed" }
func GetFieldHandler[T orm.Model](idParam string, field string) gin.HandlerFunc {
	field = nameToField(field, *new(T))

	return func(c *gin.Context) {
		var request GetRequestOptions
		if err := c.ShouldBind(&request); err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetFieldHandler: bind request failed")
			ResponseError(c, CodeBadRequest, err)
			return
		}
		options := buildQueryOptions[T](request)

		model, err := getModelByID[T](c, idParam, service.Preload(field, options...))
		if err != nil {
			logger.WithContext(c).WithError(err).
				Warn("GetFieldHandler: getModelByID failed")
			ResponseError(c, CodeProcessFailed, err)
			return
		}

		fieldValue := reflect.ValueOf(model).
			Elem(). // because model is a pointer
			FieldByName(field)

		var addition []gin.H
		if request.Total && fieldValue.Kind() == reflect.Slice {
			total, err := getAssociationCount(c, model, field, request.FilterBy, request.FilterValue)
			if err != nil {
				logger.WithContext(c).WithError(err).
					Warn("GetFieldHandler: getAssociationCount failed")
				addition = append(addition, gin.H{"totalError": err.Error()})
			} else {
				addition = append(addition, gin.H{"total": total})
			}
		}

		ResponseSuccess(c, fieldValue.Interface(), addition...)
	}
}

// buildQueryOptions 构造排序/过滤/预加载选项。order_by 与 filter_by 的列名来自 URL，
// 需过模型列白名单后才使用，避免标识符注入。分页不在此处理（见 pageOption），
// 因为 GetByID/GetField 不需要分页。
func buildQueryOptions[T any](request GetRequestOptions) []service.QueryOption {
	var options []service.QueryOption
	valid := validColumns[T]()
	if request.OrderBy != "" {
		if _, ok := valid[request.OrderBy]; ok {
			options = append(options, service.OrderBy(request.OrderBy, request.Descending))
		}
	}
	if request.FilterBy != "" && request.FilterValue != "" {
		if _, ok := valid[request.FilterBy]; ok {
			options = append(options, service.FilterBy(request.FilterBy, request.FilterValue))
		}
	}
	for _, field := range request.Preload {
		// logger.WithField("field", field).Debug("Preload field")
		options = append(options, service.Preload(field))
	}
	return options
}

// getModelByID gets idParam from url and get model from database
func getModelByID[T orm.Model](c *gin.Context, idParam string, options ...service.QueryOption) (*T, error) {
	var model T

	id := c.Param(idParam)
	if id == "" {
		logger.WithContext(c).WithField("idParam", idParam).
			Warn("getModelByID: id is empty")
		return &model, ErrMissingID
	}

	err := service.GetByID[T](c, id, &model, options...)
	return &model, err
}

func getCount[T any](ctx context.Context, filterBy string, filterValue any) (total int64, err error) {
	var option []service.QueryOption
	if filterBy != "" && filterValue != "" {
		option = append(option, service.FilterBy(filterBy, filterValue))
	}
	total, err = service.Count[T](ctx, option...)
	return total, err
}

func getAssociationCount(ctx context.Context, model any, field string, filterBy string, filterValue any) (total int64, err error) {
	var options []service.QueryOption
	if filterBy != "" && filterValue != "" {
		options = append(options, service.FilterBy(filterBy, filterValue))
	}
	count, err := service.CountAssociations(ctx, model, field, options...)
	return count, err
}
