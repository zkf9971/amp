package accounts

func (o *Organization) getMemberIndex(memberName string) int {
	memberIndex := -1
	for i, member := range o.Members {
		if member.Name == memberName {
			memberIndex = i
			break
		}
	}
	return memberIndex
}

func (o *Organization) getMember(memberName string) *OrganizationMember {
	for _, member := range o.Members {
		if member.Name == memberName {
			return member
		}
	}
	return nil
}

// HasMember returns whether the given user is an organization member
func (o *Organization) HasMember(memberName string) bool {
	return o.getMemberIndex(memberName) != -1
}

func (o *Organization) getTeam(teamName string) *Team {
	for _, team := range o.Teams {
		if team.Name == teamName {
			return team
		}
	}
	return nil
}

func (o *Organization) getTeamIndex(teamName string) int {
	teamIndex := -1
	for i, team := range o.Teams {
		if team.Name == teamName {
			teamIndex = i
			break
		}
	}
	return teamIndex
}

func (o *Organization) hasTeam(teamName string) bool {
	return o.getTeamIndex(teamName) != -1
}

func (o *Organization) getMemberTeams(name string) []*Team {
	memberTeams := []*Team{}
	for _, t := range o.Teams {
		if t.hasMember(name) {
			memberTeams = append(memberTeams, t)
		}
	}
	return memberTeams
}
