// SPDX-License-Identifier: AGPL-3.0-only

package link

import (
	"crypto/sha256"
	"errors"
	"fmt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// CustomName gives each pair of app and connection one stable link per environment.
func CustomName(app, connectionName string) string {
	sum := sha256.Sum256([]byte(app + "\x00" + connectionName))
	return fmt.Sprintf("custom-%x", sum[:16])
}

// CustomValuePrefix marks the environment-scoped names of app-specific values.
const CustomValuePrefix = "DW_CUSTOM_"

// CustomValue keeps backing names apart when apps use the same environment name.
func CustomValue(link, env string) string {
	sum := sha256.Sum256([]byte(link + "\x00" + env))
	return fmt.Sprintf("%s%X", CustomValuePrefix, sum[:16])
}

func validateToCustom(l *resourcev1.Link) error {
	if err := resource.CheckLabel("app", l.GetCustomApp(), "site"); err != nil {
		return err
	}
	if l.GetService() != "" || l.GetMeetsEnvironment() != "" || l.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_UNSPECIFIED ||
		len(l.GetAccepts()) != 0 || l.GetVersion() != "" || l.GetDatabaseName() != "" || l.GetUser() != "" || l.GetSetOnce() ||
		l.GetPrefix() != "" || len(l.GetFeatures()) != 0 || l.GetSender() != "" || l.GetSenderName() != "" {
		return errors.New("a Custom link holds only its connection, app and field mappings")
	}
	sets := l.GetSets()
	if len(sets) == 0 || len(sets) > connection.MaxCustomFields {
		return fmt.Errorf("map 1 to %d fields from the Custom connection", connection.MaxCustomFields)
	}
	names, fields := map[string]bool{}, map[string]bool{}
	linkName := CustomName(l.GetCustomApp(), l.GetConnection())
	for _, s := range sets {
		if err := variable.CheckName("environment variable", s.GetCustomEnv()); err != nil {
			return err
		}
		if !connection.ValidCustomFieldName(s.GetCustomField()) || s.GetName() != CustomValue(linkName, s.GetCustomEnv()) ||
			s.GetPart() != resourcev1.LinkPart_LINK_PART_UNSPECIFIED || s.GetTemplate() != "" || s.GetTlsOn() != "" || s.GetTlsOff() != "" {
			return errors.New("map each Custom field directly to an environment variable")
		}
		if names[s.GetCustomEnv()] || fields[s.GetCustomField()] {
			return fmt.Errorf("map %s and %s once each", s.GetCustomField(), s.GetCustomEnv())
		}
		names[s.GetCustomEnv()], fields[s.GetCustomField()] = true, true
	}
	return nil
}
