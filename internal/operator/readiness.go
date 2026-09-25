package operator

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// ServerVersioner 是 readiness 用于证明 API 可访问性的最小 discovery 能力。
type ServerVersioner interface {
	ServerVersion() (*version.Info, error)
}

// ParticipationState 记录 manager 启动已经进入 leader-election 路径。
type ParticipationState struct {
	participating atomic.Bool
}

func NewParticipationState() *ParticipationState {
	return &ParticipationState{}
}

func (s *ParticipationState) MarkParticipating() {
	s.participating.Store(true)
}

// Start 在 manager 的 non-leader runnable 组启动时记录参与状态。
func (s *ParticipationState) Start(ctx context.Context) error {
	s.MarkParticipating()
	<-ctx.Done()
	return nil
}

// NeedLeaderElection 必须为 false，使 follower 也能进入 ready 状态。
func (s *ParticipationState) NeedLeaderElection() bool {
	return false
}

// NewReadyCheck 仅在 manager 已启动且 API 当前可访问时报告 ready。
func NewReadyCheck(api ServerVersioner, state *ParticipationState) healthz.Checker {
	return func(_ *http.Request) error {
		if !state.participating.Load() {
			return fmt.Errorf("manager 尚未进入 leader election 参与状态")
		}
		if _, err := api.ServerVersion(); err != nil {
			return fmt.Errorf("Kubernetes API 不可访问: %w", err)
		}
		return nil
	}
}
