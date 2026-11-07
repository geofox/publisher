package api

import (
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/translate"
)

// The owner's settings the API reads at each request: with Settings set,
// they replace UserLanguages, Translator and PublicFeedToken (which tests
// and older wiring set directly).

// deeplFor is a translator made for one key, kept until the key changes.
type deeplFor struct {
	key string
	t   Translator
}

// translator is the translator now (nil: translation is off).
func (a *API) translator() Translator {
	if a.Settings == nil {
		return a.Translator
	}
	key := a.Settings.Current().Secret(settings.TranslateDeepLKey)
	if key == "" {
		return nil
	}
	if c := a.deepl.Load(); c != nil && c.key == key {
		return c.t
	}
	mk := a.NewTranslator
	if mk == nil {
		mk = func(key string) Translator { return translate.NewDeepL(key) }
	}
	t := mk(key)
	a.deepl.Store(&deeplFor{key: key, t: t})
	return t
}

// userLanguages is the owner's languages (never nil).
func (a *API) userLanguages() []string {
	langs := a.UserLanguages
	if a.Settings != nil {
		langs = a.Settings.Current().Strings(settings.ComposingLanguages)
	}
	if langs == nil {
		langs = []string{}
	}
	return langs
}

// feedToken is the public feed's token ("" : the feed is off).
func (a *API) feedToken() string {
	if a.Settings != nil {
		return a.Settings.Current().Secret(settings.FeedToken)
	}
	return a.PublicFeedToken
}
