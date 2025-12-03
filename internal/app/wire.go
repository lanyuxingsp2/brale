//go:build wireinject

package app

import (
	"context"

	brcfg "brale/internal/config"
	"github.com/google/wire"
)

func buildAppWithWire(ctx context.Context, cfg *brcfg.Config) (*App, error) {
	wire.Build(
		provideAppBuilder,
		wire.Bind(new(appBuilderDeps), new(*AppBuilder)),
		provideAppFromBuilder,
	)
	return nil, nil
}

type appBuilderDeps interface {
	Build(context.Context) (*App, error)
}

func provideAppFromBuilder(b appBuilderDeps, ctx context.Context) (*App, error) {
	return b.Build(ctx)
}

func provideAppBuilder(cfg *brcfg.Config) *AppBuilder {
	return NewAppBuilder(cfg)
}
