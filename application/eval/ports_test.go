package eval

import (
	"errors"
	"testing"

	domaineval "simpleAgent/domain/eval"
)

// TestExecutorErrorFailureCode 校验执行器错误能携带并暴露领域失败码。
func TestExecutorErrorFailureCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domaineval.FailureCode
	}{
		{
			name: "镜像构建失败",
			err:  NewExecutorError(domaineval.FailImageBuild, "build-image", errors.New("boom")),
			want: domaineval.FailImageBuild,
		},
		{
			name: "容器启动失败",
			err:  NewExecutorError(domaineval.FailContainerStart, "start-container", errors.New("boom")),
			want: domaineval.FailContainerStart,
		},
		{
			name: "包装后仍可识别",
			err:  errors.Join(errors.New("outer"), NewExecutorError(domaineval.FailAgentTimeout, "docker-exec-agent", errors.New("timeout"))),
			want: domaineval.FailAgentTimeout,
		},
		{
			name: "普通错误无失败码",
			err:  errors.New("plain"),
			want: "",
		},
		{
			name: "nil 构造返回 nil",
			err:  NewExecutorError(domaineval.FailImageBuild, "build-image", nil),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureCodeOf(tc.err); got != tc.want {
				t.Fatalf("FailureCodeOf()=%q，期望 %q", got, tc.want)
			}
		})
	}
}
