// Copyright 2023 Greenmask
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transformers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/greenmaskio/greenmask/internal/db/postgres/transformers/utils"
	"github.com/greenmaskio/greenmask/pkg/toolkit"
)

var RandomIntTransformerDefinition = utils.NewTransformerDefinition(
	utils.NewTransformerProperties(
		"RandomInt",
		"Generate random int value from min to max",
	),

	NewRandomIntTransformer,

	toolkit.MustNewParameterDefinition(
		"column",
		"column name",
	).SetIsColumn(toolkit.NewColumnProperties().
		SetAffected(true).
		SetAllowedColumnTypes("int2", "int4", "int8", "numeric"),
	).SetRequired(true),

	toolkit.MustNewParameterDefinition(
		"min",
		"min int value threshold",
	).SetRequired(true).
		SetLinkParameter("column").
		SetDynamicModeSupport(true),

	toolkit.MustNewParameterDefinition(
		"max",
		"max int value threshold",
	).SetRequired(true).
		SetLinkParameter("column").
		SetDynamicModeSupport(true),

	toolkit.MustNewParameterDefinition(
		"keep_null",
		"indicates that NULL values must not be replaced with transformed values",
	).SetDefaultValue(toolkit.ParamsValue("true")),
)

type RandomIntTransformer struct {
	columnName      string
	keepNull        bool
	rand            *rand.Rand
	affectedColumns map[int]string
	columnIdx       int

	columnParam   toolkit.Parameterizer
	maxParam      toolkit.Parameterizer
	minParam      toolkit.Parameterizer
	keepNullParam toolkit.Parameterizer
}

func NewRandomIntTransformer(ctx context.Context, driver *toolkit.Driver, parameters map[string]toolkit.Parameterizer) (utils.Transformer, toolkit.ValidationWarnings, error) {

	columnParam := parameters["column"]
	minParam := parameters["min"]
	maxParam := parameters["max"]
	keepNullParam := parameters["keep_null"]

	var columnName string
	//var minVal, maxVal int64
	var keepNull bool

	if err := columnParam.Scan(&columnName); err != nil {
		return nil, nil, fmt.Errorf(`unable to scan "column" param: %w`, err)
	}

	idx, _, ok := driver.GetColumnByName(columnName)
	if !ok {
		return nil, nil, fmt.Errorf("column with name %s is not found", columnName)
	}
	affectedColumns := make(map[int]string)
	affectedColumns[idx] = columnName

	//if minVal >= maxVal {
	//	return nil, toolkit.ValidationWarnings{
	//		toolkit.NewValidationWarning().
	//			AddMeta("min", minVal).
	//			AddMeta("max", maxVal).
	//			SetMsg("max value must be greater that min value"),
	//	}, nil
	//}

	if err := keepNullParam.Scan(&keepNull); err != nil {
		return nil, nil, fmt.Errorf(`unable to scan "keep_null" param: %w`, err)
	}

	return &RandomIntTransformer{
		columnName:      columnName,
		keepNull:        keepNull,
		rand:            rand.New(rand.NewSource(time.Now().UnixMicro())),
		affectedColumns: affectedColumns,
		columnIdx:       idx,

		columnParam:   columnParam,
		minParam:      minParam,
		maxParam:      maxParam,
		keepNullParam: keepNullParam,
	}, nil, nil
}

func (rit *RandomIntTransformer) GetAffectedColumns() map[int]string {
	return rit.affectedColumns
}

func (rit *RandomIntTransformer) Init(ctx context.Context) error {
	return nil
}

func (rit *RandomIntTransformer) Done(ctx context.Context) error {
	return nil
}

func (rit *RandomIntTransformer) Transform(ctx context.Context, r *toolkit.Record) (*toolkit.Record, error) {
	var minVal, maxVal int64
	err := rit.minParam.Scan(&minVal)
	if err != nil {
		return nil, fmt.Errorf(`unable to scan "min" param: %w`, err)
	}

	err = rit.maxParam.Scan(&maxVal)
	if err != nil {
		return nil, fmt.Errorf(`unable to scan "max" param: %w`, err)
	}

	if minVal >= maxVal {
		return nil, fmt.Errorf("max value must be greater than min: got min = %d max = %d", minVal, maxVal)
	}

	val, err := r.GetRawColumnValueByIdx(rit.columnIdx)
	if err != nil {
		return nil, fmt.Errorf("unable to scan value: %w", err)
	}
	if val.IsNull && rit.keepNull {
		return r, nil
	}

	if err := r.SetColumnValueByIdx(rit.columnIdx, toolkit.RandomInt(rit.rand, minVal, maxVal)); err != nil {
		return nil, fmt.Errorf("unable to set new value: %w", err)
	}
	return r, nil
}

func init() {
	utils.DefaultTransformerRegistry.MustRegister(RandomIntTransformerDefinition)
}
