package parser

import (
	"bytes"
	"common"
	"fmt"
	"time"
)

type GroupByClause struct {
	FillWithZero bool
	FillValue    *Value
	Elems        []*Value
}

func (self GroupByClause) GetGroupByTime() (*time.Duration, error) {
	for _, groupBy := range self.Elems {
		if groupBy.IsFunctionCall() {
			// TODO: check the number of arguments and return an error
			if len(groupBy.Elems) != 1 {
				return nil, common.NewQueryError(common.WrongNumberOfArguments, "time function only accepts one argument")
			}
			// TODO: check the function name
			// TODO: error checking
			arg := groupBy.Elems[0].Name
			duration, err := time.ParseDuration(arg)
			if err != nil {
				return nil, common.NewQueryError(common.InvalidArgument, fmt.Sprintf("invalid argument %s to the time function", arg))
			}
			return &duration, nil
		}
	}
	return nil, nil
}

func (self *GroupByClause) GetString() string {
	buffer := bytes.NewBufferString("")

	for idx, v := range self.Elems {
		if idx != 0 {
			fmt.Fprintf(buffer, ", ")
		}
		fmt.Fprint(buffer, v.GetString())
	}

	if self.FillWithZero {
		fmt.Fprintf(buffer, " fill(%s)", self.FillValue.GetString())
	}
	return buffer.String()
}
