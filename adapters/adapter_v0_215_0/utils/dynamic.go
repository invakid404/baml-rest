package utils

import (
	commonutils "github.com/invakid404/baml-rest/adapters/common/utils"
)

func UnwrapDynamicValue(value any) any {
	return commonutils.UnwrapDynamicValue(value)
}
