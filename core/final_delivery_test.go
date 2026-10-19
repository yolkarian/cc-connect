package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type finalDeliveryPlatform struct {
	*stubRichCardSilentPlatform
	finalMu sync.Mutex
	finals []string
	fail bool
}

func (p *finalDeliveryPlatform) FinalizeMessage(_ context.Context, _ any, content string) error {
	p.finalMu.Lock()
	defer p.finalMu.Unlock()
	p.finals=append(p.finals,content)
	if p.fail { return errors.New("continuation fragment failed after partial delivery") }
	return nil
}

type finalDeliveryAgent struct { stubAgent; live *cujAgentSession }
func (a *finalDeliveryAgent) StartSession(context.Context,string)(AgentSession,error){return a.live,nil}

func runFinalDeliveryJourney(t *testing.T) {
	t.Helper()
	p:=&finalDeliveryPlatform{stubRichCardSilentPlatform:&stubRichCardSilentPlatform{stubPlatformEngine:stubPlatformEngine{n:"test"}}}
	live:=newCUJAgentSession()
	a:=&finalDeliveryAgent{live:live}
	e:=NewEngine("final",a,[]Platform{p},"",LangEnglish)
	defer e.Stop()
	e.SetDisplayConfig(DisplayCfg{Mode:"quiet",CardMode:"rich"})
	saved:=e.sessions.GetOrCreateActive("test:alice")
	for turn:=0;turn<3;turn++ {
		answer:=fmt.Sprintf("LONG_ANSWER_%d_",turn)+strings.Repeat("正文🚀",5000)
		p.finalMu.Lock();p.fail=turn==1;p.finalMu.Unlock()
		live.mu.Lock()
		live.pendingEvents=[]Event{{Type:EventText,Content:answer},{Type:EventResult,Content:answer,Done:true}}
		live.mu.Unlock()
		p.clearSent()
		e.ReceiveMessage(p,&Message{SessionKey:"test:alice",Platform:"test",MessageID:fmt.Sprint(turn),UserID:"alice",Content:"Produce a long answer",ReplyCtx:"ctx"})
		deadline:=time.Now().Add(3*time.Second)
		for time.Now().Before(deadline) && (saved.Busy() || len(saved.GetHistory(0))<2*(turn+1)){time.Sleep(5*time.Millisecond)}
		if saved.Busy(){t.Fatal("delivery failure left conversation busy")}
		p.finalMu.Lock();count:=len(p.finals);last:="";if count>0{last=p.finals[count-1]};p.finalMu.Unlock()
		if count!=turn+1 || !strings.Contains(last,answer){t.Fatal("engine bypassed finalizer or truncated answer")}
		if strings.Contains(strings.Join(p.getSent(),"\n"),answer){t.Fatal("uncertain partial final response replayed as a new send")}
		if turn==1 && !strings.Contains(strings.Join(p.getSent(),"\n"),"/history"){t.Fatal("failed delivery had no recovery notice")}
	}
	if len(saved.GetHistory(0))!=6{t.Fatal("full answer history lost after delivery failure")}
}
